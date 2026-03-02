// Package psrp implements a Packer communicator that uses the PowerShell
// Remoting Protocol (PSRP) via go-psrp. It is intended to be imported by
// Packer builder plugins and wired into the SDK's communicator.StepConnect
// via CustomConnect["psrp"].
package psrp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/smnsjas/go-psrp/client"
	"github.com/smnsjas/go-psrpcore/messages"
	"github.com/smnsjas/go-psrpcore/serialization"
)

// Communicator implements the packer.Communicator interface using PSRP.
type Communicator struct {
	client *client.Client
	config *Config
	target string
}

func (c *Communicator) opContext() (context.Context, context.CancelFunc) {
	timeout := 2 * time.Minute
	if c.config != nil && c.config.PSRPTimeout > 0 {
		timeout = c.config.PSRPTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
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
		target: target,
	}, nil
}

// Connect establishes the PSRP connection.
func (c *Communicator) Connect(ctx context.Context) error {
	if err := c.client.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to PSRP endpoint: %w", err)
	}
	return nil
}

// reconnect closes the existing connection and establishes a new one.
// It retries with polling to handle VM reboots where the OS is temporarily
// unavailable. Retries every 10 seconds until successful or ctx is cancelled.
func (c *Communicator) reconnect(ctx context.Context) error {
	// Best-effort close of stale connection
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = c.client.Close(closeCtx)

	retryInterval := 10 * time.Second
	attempt := 0

	for {
		attempt++
		psrpConfig := c.config.ToGoPSRPConfig()
		newClient, err := client.New(c.target, psrpConfig)
		if err != nil {
			log.Printf("[DEBUG] PSRP reconnect attempt %d: failed to create client: %v", attempt, err)
		} else {
			if err := newClient.Connect(ctx); err != nil {
				log.Printf("[DEBUG] PSRP reconnect attempt %d: connection failed: %v", attempt, err)
			} else {
				log.Printf("[DEBUG] PSRP reconnected after %d attempt(s)", attempt)
				c.client = newClient
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("reconnect timed out after %d attempts (last error: %v)", attempt, err)
		case <-time.After(retryInterval):
			// Try again
		}
	}
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
		parts = append(parts, psObjectToString(r))
	}
	return strings.Join(parts, "\n")
}

// psObjectToString extracts a human-readable string from a deserialized PSRP object.
// Handles PSObject types (InformationRecord, ErrorRecord, WarningRecord, etc.) by
// extracting the actual message text rather than printing the Go struct representation.
func psObjectToString(v interface{}) string {
	obj, ok := v.(*serialization.PSObject)
	if !ok {
		return fmt.Sprintf("%v", v)
	}

	// InformationRecord (from Write-Host): message is in MessageData property.
	// The MessageData is typically a HostInformationMessage PSObject with a
	// "Message" property containing the actual text.
	if msgData, exists := obj.Properties["MessageData"]; exists {
		if inner, ok := msgData.(*serialization.PSObject); ok {
			if msg, ok := inner.Properties["Message"]; ok {
				return fmt.Sprintf("%v", msg)
			}
			if inner.ToString != "" {
				return inner.ToString
			}
		}
		return fmt.Sprintf("%v", msgData)
	}

	// ErrorRecord: extract Exception.Message and optional InvocationInfo for context.
	// ErrorRecord structure: Exception (PSObject with Message), InvocationInfo
	// (PSObject with ScriptLineNumber, PositionMessage), FullyQualifiedErrorId.
	if exception, exists := obj.Properties["Exception"]; exists {
		var errMsg string
		if exObj, ok := exception.(*serialization.PSObject); ok {
			if msg, ok := exObj.Properties["Message"]; ok {
				errMsg = fmt.Sprintf("%v", msg)
			} else if exObj.ToString != "" && exObj.ToString != "PSObject" {
				errMsg = exObj.ToString
			}
		}
		if errMsg == "" {
			errMsg = fmt.Sprintf("%v", exception)
		}
		// Append script location if available
		if invInfo, exists := obj.Properties["InvocationInfo"]; exists {
			if invObj, ok := invInfo.(*serialization.PSObject); ok {
				if pos, ok := invObj.Properties["PositionMessage"]; ok {
					posStr := fmt.Sprintf("%v", pos)
					if posStr != "" && posStr != "PSObject" {
						errMsg += "\n" + posStr
					}
				}
			}
		}
		return errMsg
	}

	// Use ToString if meaningful (not the default "PSObject")
	if obj.ToString != "" && obj.ToString != "PSObject" {
		return obj.ToString
	}

	// WarningRecord and other types with a direct Message property
	if msg, exists := obj.Properties["Message"]; exists {
		return fmt.Sprintf("%v", msg)
	}

	// Last resort
	if obj.ToString != "" {
		return obj.ToString
	}

	return fmt.Sprintf("%v", v)
}

// Start takes a RemoteCmd and starts executing it remotely.
// This is non-blocking - it returns immediately and the command runs asynchronously.
func (c *Communicator) Start(ctx context.Context, cmd *packer.RemoteCmd) error {
	const exitMarker = "__PACKER_EXIT_CODE__:"

	// Unwrap encoded commands to prevent child process hangs
	cmdString := strings.TrimSpace(cmd.Command)
	if strings.Contains(strings.ToLower(cmdString), "-encodedcommand") {
		re := regexp.MustCompile(`(?i)^powershell(?:\.exe)?\s+.*?-(?:EncodedCommand|e|enc|ec|en)\s+([A-Za-z0-9+/=]+)$`)
		matches := re.FindStringSubmatch(cmdString)
		if len(matches) == 2 {
			decoded, err := base64.StdEncoding.DecodeString(matches[1])
			if err == nil && len(decoded)%2 == 0 {
				chars := make([]uint16, len(decoded)/2)
				for i := range chars {
					chars[i] = binary.LittleEndian.Uint16(decoded[i*2 : i*2+2])
				}
				cmdString = string(utf16.Decode(chars))
				log.Printf("[DEBUG] PSRP intercepted -EncodedCommand and decoded its payload")
			}
		}
	}

	// Build normalization preamble for any .ps1 files referenced in the command.
	// PowerShell 5.1 can't parse certain constructs (try/catch, if/else) in
	// files with LF-only line endings. This normalizes them to CRLF on the VM.
	var normPreamble string
	re := regexp.MustCompile(`'([^']+\.ps1)'`)
	matches := re.FindAllStringSubmatch(cmdString, -1)
	log.Printf("[DEBUG] PSRP CRLF normalization: found %d .ps1 paths in command", len(matches))
	for _, match := range matches {
		p := strings.ReplaceAll(match[1], "'", "''")
		log.Printf("[DEBUG] PSRP CRLF normalization: will normalize %s", p)
		// Read script, normalize LF→CRLF, write back. Always normalize
		// (double-replace is safe for any input). Diagnostic output via stdout.
		normPreamble += "$__f='" + p + "'\n" +
			"$__t=[IO.File]::ReadAllText($__f)\n" +
			"$__t=$__t -replace \"`r`n\",\"`n\" -replace \"`n\",\"`r`n\"\n" +
			"[IO.File]::WriteAllText($__f,$__t,[System.Text.Encoding]::UTF8)\n"
	}

	wrappedCmd := fmt.Sprintf(`& {
%s$__packerErr = $null
try {
%s
} catch {
$__packerErr = $_
Write-Output ("ERROR: " + $_.Exception.Message)
if ($_.ScriptStackTrace) { Write-Output ("STACK: " + $_.ScriptStackTrace) }
if ($_.InvocationInfo.PositionMessage) { Write-Output $_.InvocationInfo.PositionMessage }
}
$ec = if ($__packerErr) {
	if ($LASTEXITCODE -and $LASTEXITCODE -ne 0) { $LASTEXITCODE } else { 1 }
} elseif ($?) {
	if ($LASTEXITCODE -ne $null) { $LASTEXITCODE } else { 0 }
} else {
	if ($LASTEXITCODE -ne $null) { $LASTEXITCODE } else { 1 }
}
Write-Output "%s$ec"
}`, normPreamble, cmdString, exitMarker)

	streamResult, err := c.client.ExecuteStream(ctx, wrappedCmd)
	if err != nil {
		log.Printf("[DEBUG] PSRP command failed, attempting reconnect: %v", err)
		if reconnErr := c.reconnect(ctx); reconnErr != nil {
			return fmt.Errorf("failed to start PSRP command: %w (reconnect also failed: %v)", err, reconnErr)
		}
		streamResult, err = c.client.ExecuteStream(ctx, wrappedCmd)
		if err != nil {
			return fmt.Errorf("failed to start PSRP command after reconnect: %w", err)
		}
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
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-ch:
					if !ok {
						return
					}
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
		}

		// Error channel: same as drainTo but tracks that errors occurred
		drainErrors := func(ch <-chan *messages.Message, w io.Writer) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-ch:
					if !ok {
						return
					}
					if msg == nil {
						continue
					}
					mu.Lock()
					hadErrors = true
					mu.Unlock()
					text := deserializeMessage(msg)
					log.Printf("[DEBUG] PSRP error stream message (len=%d): %q", len(text), text)
					if w != nil && text != "" {
						fmt.Fprintln(w, text)
					}
				}
			}
		}

		// Log-only drain: writes to PACKER_LOG, not Packer UI.
		// Matches WinRM behavior where verbose/debug are not transmitted.
		drainToLog := func(ch <-chan *messages.Message, prefix string) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-ch:
					if !ok {
						return
					}
					if msg == nil {
						continue
					}
					text := deserializeMessage(msg)
					if text != "" {
						for _, line := range strings.Split(text, "\n") {
							if line != "" {
								log.Printf("[DEBUG] PSRP %s: %s", prefix, line)
							}
						}
					}
				}
			}
		}

		// Drain and discard (e.g., progress records)
		drainDiscard := func(ch <-chan *messages.Message) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
				}
			}
		}

		wg.Add(7)
		go drainTo(streamResult.Output, cmd.Stdout)
		go drainErrors(streamResult.Errors, cmd.Stderr)
		go drainTo(streamResult.Warnings, cmd.Stderr)
		go drainToLog(streamResult.Verbose, "verbose")
		go drainToLog(streamResult.Debug, "debug")
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

	// Normalize LF to CRLF for PowerShell scripts. PowerShell 5.1 can't
	// parse certain constructs (try/catch, if/else) with LF-only endings.
	if strings.HasSuffix(strings.ToLower(path), ".ps1") {
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
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
		log.Printf("[DEBUG] PSRP upload failed, attempting reconnect: %v", err)
		if reconnErr := c.reconnect(ctx); reconnErr != nil {
			return fmt.Errorf("failed to upload file to %s: %w (reconnect also failed: %v)", path, err, reconnErr)
		}
		result, err = c.client.Execute(ctx, script)
		if err != nil {
			return fmt.Errorf("failed to upload file to %s after reconnect: %w", path, err)
		}
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
		log.Printf("[DEBUG] PSRP download failed, attempting reconnect: %v", err)
		if reconnErr := c.reconnect(ctx); reconnErr != nil {
			return fmt.Errorf("failed to download file from %s: %w (reconnect also failed: %v)", path, err, reconnErr)
		}
		result, err = c.client.Execute(ctx, script)
		if err != nil {
			return fmt.Errorf("failed to download file from %s after reconnect: %w", path, err)
		}
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
		log.Printf("[DEBUG] PSRP directory listing failed, attempting reconnect: %v", err)
		if reconnErr := c.reconnect(ctx); reconnErr != nil {
			return fmt.Errorf("failed to list directory contents: %w (reconnect also failed: %v)", err, reconnErr)
		}
		result, err = c.client.Execute(ctx, script)
		if err != nil {
			return fmt.Errorf("failed to list directory contents after reconnect: %w", err)
		}
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
