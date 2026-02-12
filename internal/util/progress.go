package util

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/leona/helix-assist/internal/config"
	"github.com/leona/helix-assist/internal/lsp"
)

type ProgressIndicator struct {
	svc            *lsp.Service
	enabled        bool
	updateInterval time.Duration
	spinnerFrames  []string
	ctx            context.Context
	cancel         context.CancelFunc
	startTime      time.Time
	mu             sync.Mutex
}

func NewProgressIndicator(svc *lsp.Service, cfg *config.Config) *ProgressIndicator {
	return &ProgressIndicator{
		svc:            svc,
		enabled:        cfg.EnableProgressSpinner,
		updateInterval: time.Duration(cfg.ProgressUpdateInterval) * time.Millisecond,
		spinnerFrames:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	}
}

func (p *ProgressIndicator) Start() {
	if !p.enabled {
		return
	}

	p.mu.Lock()
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.startTime = time.Now()
	p.mu.Unlock()

	p.svc.Logger.Log("AI completion started")
	go p.animate()
}

func (p *ProgressIndicator) Stop() {
	if !p.enabled {
		return
	}

	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	elapsed := time.Since(p.startTime)
	p.mu.Unlock()

	// Show final time
	p.svc.SendShowMessage(lsp.MessageTypeInfo, fmt.Sprintf("✓ AI completion (%s)", p.formatElapsed(elapsed)))
	p.svc.Logger.Log(fmt.Sprintf("AI completion finished in %s", p.formatElapsed(elapsed)))
}

func (p *ProgressIndicator) animate() {
	// Update every full second (much safer than 200ms intervals)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			elapsed := time.Since(p.startTime)
			seconds := int(elapsed.Seconds())
			p.svc.SendShowMessage(lsp.MessageTypeInfo, fmt.Sprintf("⏳ AI completion (%ds)", seconds))
		}
	}
}

func (p *ProgressIndicator) formatElapsed(duration time.Duration) string {
	seconds := duration.Seconds()
	return fmt.Sprintf("%.1fs", seconds)
}
