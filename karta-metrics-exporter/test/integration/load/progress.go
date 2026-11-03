// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

type progressSample struct {
	Timestamp time.Time     `json:"timestamp"`
	Stats     RecorderStats `json:"stats"`
}

func monitorProgress(ctx context.Context, child *exporterProcess, path string) (resultErr error) {
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create progress log: %w", err)
	}
	defer func() {
		if err := output.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close progress log: %w", err))
		}
	}()
	encoder := json.NewEncoder(output)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if ctx.Err() != nil {
				return nil
			}
			// Cancelling an active protocol request kills the child. Monitor shutdown
			// instead waits for this independently bounded request to finish.
			requestCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			response, err := child.Request(requestCtx, Command{Operation: "progress"})
			cancel()
			if err != nil {
				return fmt.Errorf("request exporter progress: %w", err)
			}
			if err := encoder.Encode(progressSample{Timestamp: time.Now().UTC(), Stats: response.Stats}); err != nil {
				return fmt.Errorf("write progress log: %w", err)
			}
		}
	}
}
