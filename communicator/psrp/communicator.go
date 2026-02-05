// Package psrp implements a Packer communicator that uses the PowerShell
// Remoting Protocol (PSRP) via go-psrp. It is intended to be imported by
// Packer builder plugins and wired into the SDK's communicator.StepConnect
// via CustomConnect["psrp"].
package psrp

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/smnsjas/go-psrp/client"
	"github.com/smnsjas/go-psrpcore/messages"
	"github.com/smnsjas/go-psrpcore/serialization"
)

// Communicator implements the packer.Communicator interface using PSRP.
type Communicator struct {
	client *client.Client
	config *Config
}

func (c *Communicator) opContext() (context.Context, context.CancelFunc) {
	if c.config != nil && c.config.PSRPTimeout > 0 {
		return context.WithTimeout(context.Background(), c.config.PSRPTimeout)
	}
	return context.WithCancel(context.Background())
}

// New creates a new PSRP communicator with the given configuration.
func New(target string, config *Config) (*Communicator, error) {
	psrpConfig := config.ToGoPSRPConfig()

	psrpClient, err := client.New(target, psrpConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create PSRP client: %w", err)
	}

	return &Communicator{
		client: psrpClient,
		config: config,
	}, nil
}

// Connect establishes the PSRP connection.
func (c *Communicator) Connect(ctx context.Context) error {
	if err := c.client.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to PSRP endpoint: %w", err)
	}
	return nil
}

// deserializeMessage extracts deserialized objects from a PSRP message.
// Returns the deserialized objects as a formatted string.
func deserializeMessage(msg *messages.Message) string {
	if msg == nil {
		return ""
	}
	deser := serialization.NewDeserializer()
	results, err := deser.Deserialize(msg.Data)
	if err != nil {
		return string(msg.Data)
	}
	var parts []string
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("%v", r))
	}
	return strings.Join(parts, "\n")
}

// Start takes a RemoteCmd and starts executing it remotely.
// This is non-blocking - it returns immediately and the command runs asynchronously.
func (c *Communicator) Start(ctx context.Context, cmd *packer.RemoteCmd) error {
	const exitMarker = "__PACKER_EXIT_CODE__:"

	wrappedCmd := fmt.Sprintf(`& {
%s
$ec = if ($?) {
	if ($LASTEXITCODE -ne $null) { $LASTEXITCODE } else { 0 }
} else {
	if ($LASTEXITCODE -ne $null) { $LASTEXITCODE } else { 1 }
}
Write-Output "%s$ec"
}`, cmd.Command, exitMarker)

	streamResult, err := c.client.ExecuteStream(ctx, wrappedCmd)
	if err != nil {
		return fmt.Errorf("failed to start PSRP command: %w", err)
	}

	go func() {
		var wg sync.WaitGroup
		var hadErrors bool
		var exitCode int
		var exitCodeSet bool
		var mu sync.Mutex

		// Helper: drain a *messages.Message channel, deserialize, write to writer
		drainTo := func(ch <-chan *messages.Message, w io.Writer) {
			defer wg.Done()
			for msg := range ch {
				if w == nil || msg == nil {
					continue
				}
				text := deserializeMessage(msg)
				if text != "" {
					lines := strings.Split(text, "\n")
					for i, line := range lines {
						if strings.HasPrefix(line, exitMarker) {
							if parsed, parseErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, exitMarker))); parseErr == nil {
								mu.Lock()
								exitCode = parsed
								exitCodeSet = true
								mu.Unlock()
							}
							continue
						}
						if i == len(lines)-1 && line == "" {
							continue
						}
						fmt.Fprintln(w, line)
					}
				}
			}
		}

		// Error channel: same as drainTo but tracks that errors occurred
		drainErrors := func(ch <-chan *messages.Message, w io.Writer) {
			defer wg.Done()
			for msg := range ch {
				if msg == nil {
					continue
				}
				mu.Lock()
				hadErrors = true
				mu.Unlock()
				if w != nil {
					text := deserializeMessage(msg)
					if text != "" {
						fmt.Fprintln(w, text)
					}
				}
			}
		}

		// Drain and discard (e.g., progress records)
		drainDiscard := func(ch <-chan *messages.Message) {
			defer wg.Done()
			for range ch {
			}
		}

		wg.Add(7)
		go drainTo(streamResult.Output, cmd.Stdout)
		go drainErrors(streamResult.Errors, cmd.Stderr)
		go drainTo(streamResult.Warnings, cmd.Stderr)
		go drainTo(streamResult.Verbose, cmd.Stdout)
		go drainTo(streamResult.Debug, cmd.Stdout)
		go drainDiscard(streamResult.Progress)
		go drainTo(streamResult.Information, cmd.Stdout)

		// Wait for pipeline completion and all streams to drain
		runErr := streamResult.Wait()
		wg.Wait()

		mu.Lock()
		finalExitCode := exitCode
		haveExitCode := exitCodeSet
		hadErrs := hadErrors
		mu.Unlock()

		if !haveExitCode {
			if runErr != nil || hadErrs {
				finalExitCode = 1
			} else {
				finalExitCode = 0
			}
		}
		cmd.SetExited(finalExitCode)
	}()

	return nil
}

// Upload uploads a file to the remote machine at the given path.
func (c *Communicator) Upload(path string, input io.Reader, fi *os.FileInfo) error {
	ctx, cancel := c.opContext()
	defer cancel()

	data, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("failed to read input data: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	escapedPath := strings.ReplaceAll(path, "'", "''")

	script := fmt.Sprintf(`
		$bytes = [System.Convert]::FromBase64String('%s')
		$parentDir = Split-Path -Parent '%s'
		if ($parentDir -and !(Test-Path $parentDir)) {
			New-Item -ItemType Directory -Path $parentDir -Force | Out-Null
		}
		[System.IO.File]::WriteAllBytes('%s', $bytes)
	`, encoded, escapedPath, escapedPath)

	result, err := c.client.Execute(ctx, script)
	if err != nil {
		return fmt.Errorf("failed to upload file to %s: %w", path, err)
	}

	if result.HadErrors {
		return fmt.Errorf("upload failed: %s", formatResultErrors(result))
	}

	return nil
}

// UploadDir uploads the contents of a directory to the remote machine.
func (c *Communicator) UploadDir(dst string, src string, exclude []string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		for _, pattern := range exclude {
			if matched, _ := filepath.Match(pattern, filepath.Base(path)); matched {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		// Use backslashes for Windows remote paths
		dstPath := dst + "\\" + strings.ReplaceAll(relPath, "/", "\\")

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", path, err)
		}
		defer file.Close()

		return c.Upload(dstPath, file, &info)
	})
}

// Download downloads a file from the remote machine.
func (c *Communicator) Download(path string, output io.Writer) error {
	ctx, cancel := c.opContext()
	defer cancel()

	escapedPath := strings.ReplaceAll(path, "'", "''")

	script := fmt.Sprintf(`
		if (!(Test-Path '%s')) {
			throw "File not found: %s"
		}
		$bytes = [System.IO.File]::ReadAllBytes('%s')
		[System.Convert]::ToBase64String($bytes)
	`, escapedPath, path, escapedPath)

	result, err := c.client.Execute(ctx, script)
	if err != nil {
		return fmt.Errorf("failed to download file from %s: %w", path, err)
	}

	if result.HadErrors {
		return fmt.Errorf("download failed: %s", formatResultErrors(result))
	}

	if len(result.Output) == 0 {
		return fmt.Errorf("no output received from download command")
	}

	// Collect all output (base64 may be split across multiple objects)
	var encodedParts []string
	for _, obj := range result.Output {
		encodedParts = append(encodedParts, fmt.Sprintf("%v", obj))
	}
	encoded := strings.TrimSpace(strings.Join(encodedParts, ""))

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("failed to decode downloaded data: %w", err)
	}

	if _, err := output.Write(decoded); err != nil {
		return fmt.Errorf("failed to write downloaded data: %w", err)
	}

	return nil
}

// DownloadDir downloads the contents of a directory from the remote machine.
func (c *Communicator) DownloadDir(src string, dst string, exclude []string) error {
	ctx, cancel := c.opContext()
	defer cancel()

	escapedSrc := strings.ReplaceAll(src, "'", "''")

	// Get relative paths of all files
	script := fmt.Sprintf(`
		Get-ChildItem -Path '%s' -Recurse -File | ForEach-Object {
			$_.FullName.Substring('%s'.Length).TrimStart('\', '/')
		}
	`, escapedSrc, escapedSrc)

	result, err := c.client.Execute(ctx, script)
	if err != nil {
		return fmt.Errorf("failed to list directory contents: %w", err)
	}

	if result.HadErrors {
		return fmt.Errorf("directory listing failed: %s", formatResultErrors(result))
	}

	for _, obj := range result.Output {
		relPath := strings.TrimSpace(fmt.Sprintf("%v", obj))
		if relPath == "" {
			continue
		}

		// Check exclusions
		excluded := false
		for _, pattern := range exclude {
			if matched, _ := filepath.Match(pattern, filepath.Base(relPath)); matched {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		remotePath := src + "\\" + strings.ReplaceAll(relPath, "/", "\\")
		localPath := filepath.Join(dst, filepath.FromSlash(strings.ReplaceAll(relPath, "\\", "/")))

		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", localPath, err)
		}

		var buf bytes.Buffer
		if err := c.Download(remotePath, &buf); err != nil {
			return fmt.Errorf("failed to download %s: %w", remotePath, err)
		}

		if err := os.WriteFile(localPath, buf.Bytes(), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", localPath, err)
		}
	}

	return nil
}

// Close closes the PSRP connection.
func (c *Communicator) Close() error {
	ctx, cancel := c.opContext()
	defer cancel()
	if err := c.client.Close(ctx); err != nil {
		return fmt.Errorf("failed to close PSRP connection: %w", err)
	}
	return nil
}

// formatResultErrors formats error objects from a client.Result into a string.
// Result.Errors is []interface{} - not a typed error type.
func formatResultErrors(result *client.Result) string {
	var errMsgs []string
	for _, psErr := range result.Errors {
		errMsgs = append(errMsgs, fmt.Sprintf("%v", psErr))
	}
	return strings.Join(errMsgs, "; ")
}
