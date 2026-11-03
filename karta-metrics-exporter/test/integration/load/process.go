// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type exporterProcess struct {
	cmd           *exec.Cmd
	input, output *os.File
	log           *os.File
	encoder       *json.Encoder
	decoder       *json.Decoder
	requests      chan struct{}
	done          chan struct{}
	waitErr       error
	closing       atomic.Bool
	closeOnce     sync.Once
	closeErr      error
}

func startExporter(ctx context.Context, executable, kubeconfig, logPath string, maxSamples, workers int, scenario string) (*exporterProcess, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create exporter log: %w", err)
	}
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		_ = inputRead.Close()
		_ = inputWrite.Close()
		_ = log.Close()
		return nil, err
	}
	cmd := exec.CommandContext(ctx, executable, "--child", "--kubeconfig", kubeconfig, "--max-samples", strconv.Itoa(maxSamples), "--workers", strconv.Itoa(workers), "--scenario", scenario)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inputRead, outputWrite, log
	if err := cmd.Start(); err != nil {
		_ = inputRead.Close()
		_ = inputWrite.Close()
		_ = outputRead.Close()
		_ = outputWrite.Close()
		_ = log.Close()
		return nil, fmt.Errorf("start exporter: %w", err)
	}
	_ = inputRead.Close()
	_ = outputWrite.Close()
	process := &exporterProcess{cmd: cmd, input: inputWrite, output: outputRead, log: log, encoder: json.NewEncoder(inputWrite), decoder: json.NewDecoder(outputRead), requests: make(chan struct{}, 1), done: make(chan struct{})}
	go func() { process.waitErr = cmd.Wait(); close(process.done) }()
	return process, nil
}

func (p *exporterProcess) PID() int { return p.cmd.Process.Pid }

func (p *exporterProcess) Request(ctx context.Context, command Command) (Response, error) {
	if p.closing.Load() {
		return Response{}, errors.New("exporter is closing")
	}
	return p.request(ctx, command, false)
}

func (p *exporterProcess) request(ctx context.Context, command Command, shutdown bool) (Response, error) {
	select {
	case p.requests <- struct{}{}:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	defer func() { <-p.requests }()
	if p.closing.Load() && !shutdown {
		return Response{}, errors.New("exporter is closing")
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	type reply struct {
		response Response
		err      error
	}
	replies := make(chan reply, 1)
	go func() {
		var response Response
		err := p.encoder.Encode(command)
		if err == nil {
			err = p.decoder.Decode(&response)
		}
		replies <- reply{response, err}
	}()
	select {
	case result := <-replies:
		if result.err != nil {
			return Response{}, fmt.Errorf("exporter protocol: %w", result.err)
		}
		if result.response.Error != "" {
			return result.response, errors.New(result.response.Error)
		}
		return result.response, nil
	case <-ctx.Done():
		// A partial command/response cannot be reused safely after cancellation.
		_ = p.cmd.Process.Kill()
		_ = p.input.Close()
		_ = p.output.Close()
		<-p.done
		<-replies
		return Response{}, ctx.Err()
	}
}

func (p *exporterProcess) Close() error {
	p.closeOnce.Do(func() {
		p.closing.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		select {
		case <-p.done:
		default:
			_, p.closeErr = p.request(ctx, Command{Operation: "stop"}, true)
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			_ = p.cmd.Process.Kill()
			<-p.done
			p.closeErr = errors.Join(p.closeErr, ctx.Err())
		}
		_ = p.input.Close()
		_ = p.output.Close()
		p.closeErr = errors.Join(p.closeErr, p.waitErr, p.log.Close())
	})
	return p.closeErr
}
