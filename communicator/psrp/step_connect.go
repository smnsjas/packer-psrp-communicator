package psrp

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

// StepConnect is a multistep Step that establishes a PSRP connection
// and stores the communicator in the state bag under the key "communicator".
//
// This step is designed to be used with the SDK's communicator.StepConnect
// via its CustomConnect map. A builder would register it like:
//
//	commStep := &communicator.StepConnect{
//	    Config: &commConfig,
//	    Host:   hostFunc,
//	    CustomConnect: map[string]multistep.Step{
//	        "psrp": &psrp.StepConnect{
//	            Config: psrpConfig,
//	            Host:   hostFunc,
//	        },
//	    },
//	}
//
// NOTE: The SDK's communicator.Config.Prepare() rejects unknown communicator
// types. Builders that use PSRP must validate the "psrp" type themselves
// before calling the SDK's Config.Prepare(), or skip calling it for the
// communicator type field.
type StepConnect struct {
	Config *Config
	Host   func(multistep.StateBag) (string, error)

	// Internal state
	comm *Communicator
}

// Run establishes the PSRP connection with retry logic.
func (s *StepConnect) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)

	// If we're being re-run (e.g., after pause_before_connecting),
	// close the previous connection first.
	if s.comm != nil {
		log.Printf("[DEBUG] Closing previous PSRP connection before reconnect")
		s.comm.Close()
		s.comm = nil
	}

	// Get the host to connect to
	host, err := s.Host(state)
	if err != nil {
		err := fmt.Errorf("error getting PSRP host: %w", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	ui.Say(fmt.Sprintf("Connecting to PSRP endpoint at %s:%d...", host, s.Config.PSRPPort))

	// Create the communicator
	s.comm, err = New(host, s.Config)
	if err != nil {
		err := fmt.Errorf("error creating PSRP communicator: %w", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	// Attempt connection with retry logic
	timeout := s.Config.PSRPTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	retryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ui.Say(fmt.Sprintf("Waiting for PSRP to become available (timeout: %v)...", timeout))

	err = s.waitForPSRP(retryCtx, ui)
	if err != nil {
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	ui.Say("Connected to PSRP!")

	// Store the communicator in state for provisioners to use
	state.Put("communicator", s.comm)

	return multistep.ActionContinue
}

// waitForPSRP attempts to connect with retry logic until successful or timeout.
func (s *StepConnect) waitForPSRP(ctx context.Context, ui packersdk.Ui) error {
	var lastErr error
	retryDelay := 5 * time.Second
	maxRetryDelay := 30 * time.Second
	attempt := 0

	ticker := time.NewTicker(retryDelay)
	defer ticker.Stop()

	// Try immediately first
	if err := s.comm.Connect(ctx); err == nil {
		return nil
	} else {
		lastErr = err
		log.Printf("[DEBUG] Initial PSRP connection failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("timeout waiting for PSRP (last error: %w)", lastErr)
			}
			return fmt.Errorf("timeout waiting for PSRP")

		case <-ticker.C:
			attempt++
			ui.Message(fmt.Sprintf("Attempting PSRP connection (attempt %d)...", attempt))

			err := s.comm.Connect(ctx)
			if err == nil {
				return nil // Success!
			}

			lastErr = err
			log.Printf("[DEBUG] PSRP connection attempt %d failed: %v", attempt, err)

			// Exponential backoff with max delay
			retryDelay = retryDelay * 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
			ticker.Reset(retryDelay)
		}
	}
}

// Cleanup closes the PSRP connection if it was established.
func (s *StepConnect) Cleanup(state multistep.StateBag) {
	if s.comm != nil {
		ui := state.Get("ui").(packersdk.Ui)
		ui.Say("Closing PSRP connection...")

		if err := s.comm.Close(); err != nil {
			ui.Error(fmt.Sprintf("Error closing PSRP connection: %s", err))
		}
		s.comm = nil
	}
}
