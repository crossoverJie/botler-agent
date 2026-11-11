package wechat

import (
	"context"
	"fmt"
	"sync"
	"time"

	ilink "github.com/openilink/openilink-sdk-go"
)

// WebUI-driven QR pairing.
//
// The CLI path (Login) is a blocking loop printing to the terminal; the WebUI needs the same flow
// as a queryable singleton it can poll over HTTP. This file owns one "pairing session" state
// machine: StartPairing fetches a QR and kicks off a background poll loop, PairStatus reports
// progress, CancelPairing aborts it. On `confirmed` the loop applies the new account and brings
// the monitor online live (no restart).
//
// The state machine is a factory with injectable fetch/poll/onConfirmed/now so transitions are
// unit-testable without the network.

// PairState is the pairing state machine's state.
type PairState string

// Pairing states.
const (
	PairIdle      PairState = "idle"
	PairPending   PairState = "pending"
	PairScanned   PairState = "scanned"
	PairConfirmed PairState = "confirmed"
	PairExpired   PairState = "expired"
	PairError     PairState = "error"
)

const (
	// PairTTLMs is the overall pairing deadline (matches the CLI's 8-minute timeout).
	PairTTLMs int64 = 480_000
	// MaxQRRefreshCount caps QR refreshes in one continuous pairing flow.
	MaxQRRefreshCount = 3
	// DefaultILinkBotType is the bot_type parameter used for QR login.
	DefaultILinkBotType = "3"

	pairPollInterval = 1 * time.Second
	pairLoginBaseURL = DefaultBaseURL
)

// PairSession is the WebUI-visible pairing session.
type PairSession struct {
	State PairState
	// QRCode is the poll id passed to get_qrcode_status.
	QRCode string
	// Content is qrcode_img_content — the URL/content string to encode into a QR image.
	Content string
	BaseURL string
	// ScannedAt is when the user scanned (0 = not scanned yet).
	ScannedAt *int64
	// ExpiresAt is the overall pairing deadline (epoch ms).
	ExpiresAt int64
	// RefreshCount is the number of QR refreshes so far in THIS continuous pairing flow.
	RefreshCount int
	Error        string
}

// PairStart is the result of StartPairing.
type PairStart struct {
	Content string
	State   PairState
}

// PairStatusInfo is the JSON shape the WebUI pairing status endpoint returns.
type PairStatusInfo struct {
	State        PairState `json:"state"`
	RefreshCount *int      `json:"refreshCount,omitempty"`
	ScannedAt    *int64    `json:"scannedAt,omitempty"`
	Error        *string   `json:"error,omitempty"`
}

// PairDeps are the injectable side effects of a pairing session.
type PairDeps struct {
	FetchQR     func(ctx context.Context, apiBaseURL, botType string) (*ilink.QRCodeResponse, error)
	Poll        func(ctx context.Context, apiBaseURL, qrcode string) (*ilink.QRStatusResponse, error)
	OnConfirmed func(update AccountUpdate)
	Now         func() int64
	// Auto spawns the background poll loop on Start (tests set false and drive Step manually).
	Auto bool
}

// Pairing is one pairing state machine.
type Pairing struct {
	mu      sync.Mutex
	deps    PairDeps
	session *PairSession
	epoch   int
}

// NewPairing builds a pairing state machine, filling in default deps.
func NewPairing(deps PairDeps) *Pairing {
	def := defaultPairDeps()
	if deps.FetchQR == nil {
		deps.FetchQR = def.FetchQR
	}
	if deps.Poll == nil {
		deps.Poll = def.Poll
	}
	if deps.OnConfirmed == nil {
		deps.OnConfirmed = def.OnConfirmed
	}
	if deps.Now == nil {
		deps.Now = def.Now
	}
	return &Pairing{deps: deps}
}

func (p *Pairing) now() int64 { return p.deps.Now() }

// ComputeInheritedRefreshCount inherits the refresh count of an expired session so the total stays
// capped at MaxQRRefreshCount across the whole pairing flow.
func ComputeInheritedRefreshCount(prev *PairSession) int {
	if prev != nil && prev.State == PairExpired {
		return prev.RefreshCount
	}
	return 0
}

// Start fetches a QR and starts polling. It returns the content to encode and the resulting state.
func (p *Pairing) Start(ctx context.Context) (PairStart, error) {
	p.mu.Lock()
	p.epoch++ // a new start supersedes any in-flight loop
	epoch := p.epoch
	refreshCount := ComputeInheritedRefreshCount(p.session)
	if refreshCount > MaxQRRefreshCount {
		p.session = &PairSession{
			State:        PairError,
			BaseURL:      pairLoginBaseURL,
			ExpiresAt:    p.now() + PairTTLMs,
			RefreshCount: refreshCount,
			Error:        fmt.Sprintf("QR code expired %d times; start a new pairing to retry", MaxQRRefreshCount),
		}
		p.mu.Unlock()
		return PairStart{State: PairError}, nil
	}
	p.mu.Unlock()

	initial, err := p.deps.FetchQR(ctx, pairLoginBaseURL, DefaultILinkBotType)
	if err != nil {
		return PairStart{}, err
	}

	p.mu.Lock()
	p.session = &PairSession{
		State:        PairPending,
		QRCode:       initial.QRCode,
		Content:      initial.QRCodeImgContent,
		BaseURL:      pairLoginBaseURL,
		ExpiresAt:    p.now() + PairTTLMs,
		RefreshCount: refreshCount,
	}
	p.mu.Unlock()

	if p.deps.Auto {
		go p.autoLoop(ctx, epoch)
	}
	return PairStart{Content: initial.QRCodeImgContent, State: PairPending}, nil
}

// Cancel aborts any in-flight pairing and resets to idle.
func (p *Pairing) Cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.epoch++ // abort any in-flight loop
	p.session = nil
}

// Status returns a snapshot of the current session (nil = idle).
func (p *Pairing) Status() *PairSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == nil {
		return nil
	}
	out := *p.session
	return &out
}

// Step runs one poll + transition. It returns true when the loop should stop (terminal state).
func (p *Pairing) Step(ctx context.Context) (bool, error) {
	p.mu.Lock()
	cur := p.session
	if cur == nil || (cur.State != PairPending && cur.State != PairScanned) {
		p.mu.Unlock()
		return true, nil
	}
	if p.now() > cur.ExpiresAt {
		cur.State = PairExpired
		cur.RefreshCount++
		cur.Error = "QR code expired (no scan within 8 minutes)"
		p.mu.Unlock()
		return true, nil
	}
	baseURL, qrcode := cur.BaseURL, cur.QRCode
	p.mu.Unlock()

	resp, err := p.deps.Poll(ctx, baseURL, qrcode)
	if err != nil {
		return false, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != cur {
		return true, nil // superseded while the poll was in flight
	}
	state := p.applyTransition(cur, resp)
	return state == PairConfirmed || state == PairExpired || state == PairError, nil
}

// applyTransition applies one poll response to the session (caller holds the lock).
func (p *Pairing) applyTransition(cur *PairSession, resp *ilink.QRStatusResponse) PairState {
	switch resp.Status {
	case "wait":
		cur.State = PairPending
	case "scaned":
		if cur.State != PairScanned {
			cur.State = PairScanned
			ts := p.now()
			cur.ScannedAt = &ts
		}
	case "scaned_but_redirect":
		if resp.RedirectHost != "" {
			cur.BaseURL = "https://" + resp.RedirectHost
		}
	case "expired":
		cur.State = PairExpired
		cur.RefreshCount++
		cur.Error = "QR code expired"
	case "confirmed":
		if resp.BotToken == "" || resp.ILinkBotID == "" {
			cur.State = PairError
			cur.Error = "Login confirmed but server did not return token / bot id"
			break
		}
		// A confirmed login applies the account (disk writes). A failure here must not crash the
		// process via the unhandled background loop — surface it as an error state instead.
		func() {
			defer func() {
				if r := recover(); r != nil {
					cur.State = PairError
					cur.Error = fmt.Sprint(r)
				}
			}()
			p.deps.OnConfirmed(AccountUpdate{Token: resp.BotToken, BaseURL: resp.BaseURL, UserID: resp.ILinkUserID})
			cur.State = PairConfirmed
		}()
	}
	return cur.State
}

func (p *Pairing) autoLoop(ctx context.Context, epoch int) {
	for {
		if !p.isCurrent(epoch) {
			return
		}
		done, err := p.Step(ctx)
		if err != nil {
			warnf("[wechat] pairing poll error: %v", err)
		}
		if !p.isCurrent(epoch) {
			return
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pairPollInterval):
		}
	}
}

func (p *Pairing) isCurrent(epoch int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.epoch == epoch
}

func defaultPairDeps() PairDeps {
	return PairDeps{
		FetchQR: func(ctx context.Context, apiBaseURL, botType string) (*ilink.QRCodeResponse, error) {
			client := ilink.NewClient("", ilink.WithBaseURL(apiBaseURL), ilink.WithBotType(botType))
			return client.FetchQRCode(ctx)
		},
		Poll: func(ctx context.Context, apiBaseURL, qrcode string) (*ilink.QRStatusResponse, error) {
			client := ilink.NewClient("", ilink.WithBaseURL(apiBaseURL))
			resp, err := client.PollQRStatus(ctx, qrcode, apiBaseURL)
			if err != nil {
				// Network / gateway errors are normal during long-poll: keep waiting.
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				warnf("[wechat] pollQRStatus network error, retry: %v", err)
				return &ilink.QRStatusResponse{Status: "wait"}, nil
			}
			return resp, nil
		},
		OnConfirmed: defaultOnConfirmed,
		Now:         func() int64 { return nowMs() },
		Auto:        true,
	}
}

// defaultOnConfirmed applies the account and brings the monitor online (the WebUI re-pair path).
func defaultOnConfirmed(update AccountUpdate) {
	ApplyNewAccount(update)
	EnsureMonitor(context.Background())
}

var (
	pairingOnce sync.Once
	pairingInst *Pairing
)

// DefaultPairing returns the process-wide pairing singleton used by the WebUI.
func DefaultPairing() *Pairing {
	pairingOnce.Do(func() { pairingInst = NewPairing(PairDeps{Auto: true}) })
	return pairingInst
}

// StartPairing starts (or replaces) the singleton pairing session.
func StartPairing(ctx context.Context) (PairStart, error) { return DefaultPairing().Start(ctx) }

// CancelPairing aborts the current pairing session (back to idle).
func CancelPairing() { DefaultPairing().Cancel() }

// GetPairSession returns a snapshot of the current pairing session (nil when idle).
func GetPairSession() *PairSession { return DefaultPairing().Status() }

// PairStatus returns the WebUI-shaped pairing status.
func PairStatus() PairStatusInfo {
	s := GetPairSession()
	if s == nil {
		return PairStatusInfo{State: PairIdle}
	}
	out := PairStatusInfo{State: s.State}
	refresh := s.RefreshCount
	out.RefreshCount = &refresh
	out.ScannedAt = s.ScannedAt
	if s.Error != "" {
		errText := s.Error
		out.Error = &errText
	}
	return out
}
