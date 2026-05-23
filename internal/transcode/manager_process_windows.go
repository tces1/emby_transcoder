//go:build windows

package transcode

import (
	"io"
	"time"
)

func (p *execProcess) StopWithGrace(grace time.Duration) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if p.Done() {
		return nil
	}
	if p.stdin != nil {
		_, _ = io.WriteString(p.stdin, "q\n")
		_ = p.stdin.Close()
	}
	if p.waitForExit(grace) {
		return nil
	}
	err := p.cmd.Process.Kill()
	if p.waitForExit(2 * time.Second) {
		return nil
	}
	return err
}

func (p *execProcess) Pause() error {
	p.paused.Store(true)
	return nil
}

func (p *execProcess) Resume() error {
	p.paused.Store(false)
	return nil
}
