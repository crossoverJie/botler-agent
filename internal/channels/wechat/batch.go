package wechat

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
)

// Image batching state machine for the WeChat channel.
//
// WeChat delivers a selected photo immediately while the user may still be typing the caption
// ("选图即发，文字后到"), so image-bearing messages are held for a short window; a following text
// from the same sender joins the batch and is dispatched as ONE task (caption + vision), otherwise
// the image(s) alone are dispatched when the window expires. Multiple photos in the window also
// merge into one batch.
//
// The coordinator is pure: it owns the batch state and the routing decision only. The caller (the
// monitor) owns the flush timer and the actual dispatch/send, so window/merge/flush logic is
// unit-testable without the network or the dispatch chain.

// PendingPiece is one inbound message handed to the coordinator.
type PendingPiece struct {
	// Text is the extracted user text (may be empty for a pure image message).
	Text string
	// Images are the decoded inbound images.
	Images []agentrt.InboundImage
	// ContextToken is the latest context_token of the messages in this piece.
	ContextToken string
}

// BatchDispatch is a ready-to-run task: joined text plus all buffered images.
type BatchDispatch struct {
	// Text is the joined user texts, or the "[图片]" placeholder when the batch has no text.
	Text string
	// Images are the merged inbound images.
	Images []agentrt.InboundImage
	// ContextToken is echoed in the reply.
	ContextToken string
	// ID is a deterministic dedup id derived from the content.
	ID string
}

// ActionKind distinguishes "buffer it" from "dispatch now".
type ActionKind int

const (
	// ActionBuffer means the message was held; the caller must (re)arm the flush timer.
	ActionBuffer ActionKind = iota
	// ActionDispatch means run the returned task now.
	ActionDispatch
)

// BatchAction is the coordinator's decision for one message.
type BatchAction struct {
	Kind     ActionKind
	Dispatch BatchDispatch
}

type pendingBatch struct {
	images       []agentrt.InboundImage
	texts        []string
	contextToken string
}

// ImageBatchCoordinator owns the per-sender merge / flush decisions.
type ImageBatchCoordinator struct {
	mu       sync.Mutex
	windowMs int
	batches  map[string]*pendingBatch
}

// NewImageBatchCoordinator builds a coordinator with the given window in milliseconds
// (0 disables batching).
func NewImageBatchCoordinator(windowMs int) (*ImageBatchCoordinator, error) {
	if windowMs < 0 {
		return nil, fmt.Errorf("windowMs must be a non-negative number")
	}
	return &ImageBatchCoordinator{windowMs: windowMs, batches: map[string]*pendingBatch{}}, nil
}

// WindowMs returns the batching window.
func (c *ImageBatchCoordinator) WindowMs() int { return c.windowMs }

// OnMessage feeds one inbound message from a sender.
//
// It returns ActionBuffer when an image message was held (the caller must re-arm the flush timer
// for windowMs), and ActionDispatch when the task should run now (standalone text, a caption that
// joined a pending image batch, or an image dispatch when batching is disabled).
func (c *ImageBatchCoordinator) OnMessage(sender string, piece PendingPiece) BatchAction {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(piece.Images) > 0 && c.windowMs > 0 {
		existing, ok := c.batches[sender]
		if ok {
			existing.images = append(existing.images, piece.Images...)
			if piece.Text != "" {
				existing.texts = append(existing.texts, piece.Text)
			}
			if piece.ContextToken != "" {
				existing.contextToken = piece.ContextToken
			}
		} else {
			batch := &pendingBatch{images: append([]agentrt.InboundImage{}, piece.Images...), contextToken: piece.ContextToken}
			if piece.Text != "" {
				batch.texts = []string{piece.Text}
			}
			c.batches[sender] = batch
		}
		return BatchAction{Kind: ActionBuffer}
	}

	// Text-only message (or batching disabled): if a pending image batch exists from this sender,
	// the text is the caption — join and dispatch as one task now.
	if piece.Text != "" {
		if existing, ok := c.batches[sender]; ok {
			existing.texts = append(existing.texts, piece.Text)
			if piece.ContextToken != "" {
				existing.contextToken = piece.ContextToken
			}
			return BatchAction{Kind: ActionDispatch, Dispatch: c.take(sender, existing)}
		}
	}

	// Standalone text, or image dispatch when batching is disabled.
	return BatchAction{Kind: ActionDispatch, Dispatch: c.build(sender, piece, false)}
}

// Flush flushes a sender's pending batch (the flush timer fired with no caption).
func (c *ImageBatchCoordinator) Flush(sender string) (BatchDispatch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.batches[sender]
	if !ok {
		return BatchDispatch{}, false
	}
	return c.take(sender, existing), true
}

// HasPending reports whether a sender currently has a buffered batch.
func (c *ImageBatchCoordinator) HasPending(sender string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.batches[sender]
	return ok
}

// PendingImageCount returns how many images are currently buffered for a sender.
func (c *ImageBatchCoordinator) PendingImageCount(sender string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if batch, ok := c.batches[sender]; ok {
		return len(batch.images)
	}
	return 0
}

// PendingCount returns the number of senders with a buffered batch.
func (c *ImageBatchCoordinator) PendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.batches)
}

func (c *ImageBatchCoordinator) take(sender string, batch *pendingBatch) BatchDispatch {
	delete(c.batches, sender)
	text := ""
	for i, t := range batch.texts {
		if i > 0 {
			text += "\n"
		}
		text += t
	}
	return c.build(sender, PendingPiece{Text: text, Images: batch.images, ContextToken: batch.contextToken}, true)
}

func (c *ImageBatchCoordinator) build(sender string, piece PendingPiece, isBatch bool) BatchDispatch {
	// Image-only message: keep a placeholder so greeting/routing/logs still function.
	safeText := piece.Text
	if safeText == "" {
		safeText = "[图片]"
	}
	imageSig := ""
	if len(piece.Images) > 0 {
		h := md5.New()
		for _, img := range piece.Images {
			_, _ = h.Write(img.Data)
		}
		imageSig = hex.EncodeToString(h.Sum(nil))[:8]
	}
	// Short text signature so different captions on the same photo don't collide.
	textSig := safeText
	if r := []rune(textSig); len(r) > 20 {
		textSig = string(r[:20])
	}
	id := sender + ":" + safeText + ":" + imageSig
	if isBatch {
		id = sender + ":batch:" + imageSig + ":" + textSig
	}
	return BatchDispatch{Text: safeText, Images: piece.Images, ContextToken: piece.ContextToken, ID: id}
}
