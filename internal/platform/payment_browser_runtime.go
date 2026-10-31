package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"
)

type nodePayment struct {
	events    chan paymentEvent
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	mu        sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
}

func (p *nodePayment) Events() <-chan paymentEvent { return p.events }
func (p *nodePayment) Send(v paymentCommand) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return json.NewEncoder(p.stdin).Encode(v)
}
func (p *nodePayment) Close() {
	p.closeOnce.Do(func() {
		p.stdin.Close() // EOF asks the runtime to close its browser before process termination.
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			p.cancel()
		}
	})
}

func startPaymentNode(ctx context.Context, in paymentInput) (paymentProcess, error) {
	parent := ctx
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", paymentRuntimePath())
	cmd.Stderr = io.Discard // Browser exceptions can contain card or proxy values.
	stdin, e := cmd.StdinPipe()
	if e != nil {
		cancel()
		return nil, errors.New("runtime unavailable")
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		cancel()
		return nil, errors.New("runtime unavailable")
	}
	if e = cmd.Start(); e != nil {
		cancel()
		return nil, errors.New("runtime unavailable")
	}
	p := &nodePayment{events: make(chan paymentEvent, 16), stdin: stdin, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.events)
		defer close(p.done)
		defer cancel()
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 4096), 16384)
		for scan.Scan() {
			var v paymentEvent
			if json.Unmarshal(scan.Bytes(), &v) != nil {
				continue
			}
			select {
			case p.events <- v:
			case <-ctx.Done():
			}
		}
		cmd.Wait()
	}()
	go func() {
		select {
		case <-parent.Done():
			p.Close()
		case <-p.done:
		}
	}()
	if e = json.NewEncoder(stdin).Encode(in); e != nil {
		p.Close()
		return nil, errors.New("runtime unavailable")
	}
	return p, nil
}
