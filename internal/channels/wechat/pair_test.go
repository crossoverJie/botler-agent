package wechat

import (
	"context"
	"errors"
	"strings"
	"testing"

	ilink "github.com/openilink/openilink-sdk-go"
)

const fetchQRContent = "content1"

func fakeFetchQR(_ context.Context, _, _ string) (*ilink.QRCodeResponse, error) {
	return &ilink.QRCodeResponse{QRCode: "q1", QRCodeImgContent: fetchQRContent}, nil
}

// makePoll replays a fixed sequence of statuses, holding the last one forever.
func makePoll(seq []ilink.QRStatusResponse) func(context.Context, string, string) (*ilink.QRStatusResponse, error) {
	i := 0
	return func(context.Context, string, string) (*ilink.QRStatusResponse, error) {
		idx := i
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		if idx < 0 {
			return &ilink.QRStatusResponse{Status: "wait"}, nil
		}
		i++
		resp := seq[idx]
		return &resp, nil
	}
}

func TestPairingPendingScannedConfirmed(t *testing.T) {
	var applied []AccountUpdate
	machine := NewPairing(PairDeps{
		FetchQR: fakeFetchQR,
		Poll: makePoll([]ilink.QRStatusResponse{
			{Status: "scaned"},
			{Status: "confirmed", BotToken: "tok", ILinkBotID: "bot1", BaseURL: "https://x", ILinkUserID: "u1"},
		}),
		OnConfirmed: func(u AccountUpdate) { applied = append(applied, u) },
		Now:         func() int64 { return 0 },
	})

	res, err := machine.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != PairPending || res.Content != fetchQRContent {
		t.Fatalf("start = %+v", res)
	}
	if done, _ := machine.Step(context.Background()); done {
		t.Fatal("scaned must not be terminal")
	}
	if s := machine.Status(); s.State != PairScanned || s.ScannedAt == nil {
		t.Fatalf("status = %+v", s)
	}
	if done, _ := machine.Step(context.Background()); !done {
		t.Fatal("confirmed must be terminal")
	}
	if s := machine.Status(); s.State != PairConfirmed {
		t.Fatalf("state = %s", s.State)
	}
	if len(applied) != 1 || applied[0] != (AccountUpdate{Token: "tok", BaseURL: "https://x", UserID: "u1"}) {
		t.Fatalf("applied = %+v", applied)
	}
}

func TestPairingWaitKeepsPending(t *testing.T) {
	machine := NewPairing(PairDeps{FetchQR: fakeFetchQR, Poll: makePoll([]ilink.QRStatusResponse{{Status: "wait"}}), OnConfirmed: func(AccountUpdate) {}, Now: func() int64 { return 0 }})
	if _, err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done, _ := machine.Step(context.Background()); done {
		t.Fatal("wait must not be terminal")
	}
	if s := machine.Status(); s.State != PairPending {
		t.Fatalf("state = %s", s.State)
	}
}

func TestPairingRedirectSwitchesBaseURL(t *testing.T) {
	machine := NewPairing(PairDeps{FetchQR: fakeFetchQR, Poll: makePoll([]ilink.QRStatusResponse{{Status: "scaned_but_redirect", RedirectHost: "redirect.example.com"}}), OnConfirmed: func(AccountUpdate) {}, Now: func() int64 { return 0 }})
	if _, err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done, _ := machine.Step(context.Background()); done {
		t.Fatal("redirect must not be terminal")
	}
	s := machine.Status()
	if s.BaseURL != "https://redirect.example.com" || s.State != PairPending {
		t.Fatalf("status = %+v", s)
	}
}

func TestPairingConfirmedWithoutCredentialsIsError(t *testing.T) {
	applied := false
	machine := NewPairing(PairDeps{FetchQR: fakeFetchQR, Poll: makePoll([]ilink.QRStatusResponse{{Status: "confirmed"}}), OnConfirmed: func(AccountUpdate) { applied = true }, Now: func() int64 { return 0 }})
	if _, err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done, _ := machine.Step(context.Background()); !done {
		t.Fatal("error must be terminal")
	}
	if s := machine.Status(); s.State != PairError || applied {
		t.Fatalf("status = %+v applied=%v", s, applied)
	}
}

func TestPairingConfirmedCallbackErrorDoesNotCrash(t *testing.T) {
	machine := NewPairing(PairDeps{
		FetchQR:     fakeFetchQR,
		Poll:        makePoll([]ilink.QRStatusResponse{{Status: "confirmed", BotToken: "tok", ILinkBotID: "bot1"}}),
		OnConfirmed: func(AccountUpdate) { panic("disk full") },
		Now:         func() int64 { return 0 },
	})
	if _, err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, err := machine.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("error must be terminal")
	}
	s := machine.Status()
	if s.State != PairError || s.Error != "disk full" {
		t.Fatalf("status = %+v", s)
	}
}

func TestPairingExpiredRefreshCountIsInheritedAndCapped(t *testing.T) {
	machine := NewPairing(PairDeps{FetchQR: fakeFetchQR, Poll: makePoll([]ilink.QRStatusResponse{{Status: "expired"}}), OnConfirmed: func(AccountUpdate) {}, Now: func() int64 { return 0 }})
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := machine.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if s := machine.Status(); s.RefreshCount != i {
			t.Fatalf("round %d: refreshCount = %d, want %d", i, s.RefreshCount, i)
		}
		if done, _ := machine.Step(ctx); !done {
			t.Fatal("expired must be terminal")
		}
		if s := machine.Status(); s.RefreshCount != i+1 {
			t.Fatalf("round %d: refreshCount = %d, want %d", i, s.RefreshCount, i+1)
		}
	}
	res, err := machine.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != PairError {
		t.Fatalf("state = %s, want error", res.State)
	}
	if s := machine.Status(); s.State != PairError || s.Error == "" {
		t.Fatalf("status = %+v", s)
	}
}

func TestPairingTTLTimeoutExpires(t *testing.T) {
	var now int64
	machine := NewPairing(PairDeps{FetchQR: fakeFetchQR, Poll: makePoll([]ilink.QRStatusResponse{{Status: "wait"}}), OnConfirmed: func(AccountUpdate) {}, Now: func() int64 { return now }})
	if _, err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = PairTTLMs + 1
	if done, _ := machine.Step(context.Background()); !done {
		t.Fatal("TTL expiry must be terminal")
	}
	s := machine.Status()
	if s.State != PairExpired || s.RefreshCount != 1 {
		t.Fatalf("status = %+v", s)
	}
}

func TestPairingCancelResetsToIdle(t *testing.T) {
	machine := NewPairing(PairDeps{FetchQR: fakeFetchQR, Poll: makePoll([]ilink.QRStatusResponse{{Status: "wait"}}), OnConfirmed: func(AccountUpdate) {}, Now: func() int64 { return 0 }})
	if _, err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	machine.Cancel()
	if s := machine.Status(); s != nil {
		t.Fatalf("status = %+v, want nil", s)
	}
}

func TestPairingPropagatesPollError(t *testing.T) {
	machine := NewPairing(PairDeps{
		FetchQR: fakeFetchQR,
		Poll: func(context.Context, string, string) (*ilink.QRStatusResponse, error) {
			return nil, errors.New("boom")
		},
		OnConfirmed: func(AccountUpdate) {},
		Now:         func() int64 { return 0 },
	})
	if _, err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Step(context.Background()); err == nil {
		t.Fatal("want the poll error to surface")
	}
}

func TestComputeInheritedRefreshCount(t *testing.T) {
	base := &PairSession{State: PairPending, RefreshCount: 5}
	if got := ComputeInheritedRefreshCount(nil); got != 0 {
		t.Fatalf("nil = %d", got)
	}
	for _, state := range []PairState{PairPending, PairScanned, PairConfirmed, PairError, PairIdle} {
		clone := *base
		clone.State = state
		if got := ComputeInheritedRefreshCount(&clone); got != 0 {
			t.Fatalf("%s = %d, want 0", state, got)
		}
	}
	expired := *base
	expired.State = PairExpired
	if got := ComputeInheritedRefreshCount(&expired); got != 5 {
		t.Fatalf("expired = %d, want 5", got)
	}
}

func TestRenderPairQRIsPNGDataURL(t *testing.T) {
	url, err := RenderPairQR("https://example.com/some-content")
	if err != nil {
		t.Fatal(err)
	}
	if !regexpHasPNGPrefix(url) {
		t.Fatalf("url does not start with a PNG data URL: %.40q", url)
	}
}

func regexpHasPNGPrefix(s string) bool {
	return strings.HasPrefix(s, "data:image/png;base64,")
}

func TestRenderASCIIQR(t *testing.T) {
	art, err := renderASCIIQR("https://example.com/some-content")
	if err != nil {
		t.Fatal(err)
	}
	if len(art) < 100 {
		t.Fatalf("ascii QR too short: %q", art)
	}
}
