import crypto from "node:crypto";
import type { InboundImage } from "./download.ts";

/**
 * Per-sender image batching state machine for the WeChat channel.
 *
 * WeChat delivers a selected photo immediately while the user may still be typing the caption
 * ("选图即发,文字后到"), so image-bearing messages are held for a short window; a following text
 * from the same sender joins the batch and is dispatched as ONE task (caption + vision);
 * otherwise the image(s) alone are dispatched when the window expires. Multiple photos in the
 * window also merge into one batch.
 *
 * This class is pure: it owns the batch state and the routing decision only. The caller (the
 * monitor) owns the flush timer and the actual dispatch/send, so the window/merge/flush logic
 * is unit-testable without the network or the dispatch chain.
 */

export interface PendingPiece {
	/** Extracted user text (may be empty for a pure image message). */
	text: string;
	/** Decoded inbound images. */
	images: InboundImage[];
	/** Latest context_token of the messages in this piece; echoed in the reply. */
	contextToken?: string;
}

/** A ready-to-run task: joined text + all buffered images. */
export interface BatchDispatch {
	/** Joined user texts, or the "[图片]" placeholder when the batch has no text. */
	text: string;
	images: InboundImage[];
	contextToken?: string;
	/** Deterministic dedup id derived from the content (caller may override with message_id). */
	id: string;
}

export type BatchAction =
	| { kind: "dispatch"; dispatch: BatchDispatch }
	| { kind: "buffer" };

interface PendingBatch {
	images: InboundImage[];
	texts: string[];
	contextToken?: string;
}

export class ImageBatchCoordinator {
	private readonly windowMs: number;
	private readonly batches = new Map<string, PendingBatch>();

	constructor(windowMs: number) {
		if (!Number.isFinite(windowMs) || windowMs < 0) throw new Error("windowMs must be a non-negative number");
		this.windowMs = windowMs;
	}

	/**
	 * Feed one inbound message from a sender.
	 *
	 * Returns:
	 * - `{ kind: "buffer" }` — image message held; the caller must (re)arm the flush timer for
	 *   `windowMs` so the batch is eventually flushed if no caption arrives.
	 * - `{ kind: "dispatch", dispatch }` — run this task now (standalone text, or a caption that
	 *   joined a pending image batch, or an image dispatch when batching is disabled).
	 */
	onMessage(sender: string, piece: PendingPiece): BatchAction {
		const { text, images, contextToken } = piece;

		if (images.length > 0 && this.windowMs > 0) {
			const existing = this.batches.get(sender);
			if (existing) {
				existing.images.push(...images);
				if (text) existing.texts.push(text);
				if (contextToken) existing.contextToken = contextToken;
			} else {
				this.batches.set(sender, {
					images: [...images],
					texts: text ? [text] : [],
					contextToken,
				});
			}
			return { kind: "buffer" };
		}

		// Text-only message (or batching disabled): if a pending image batch exists from this
		// sender, the text is the caption — join and dispatch as one task now.
		if (text) {
			const existing = this.batches.get(sender);
			if (existing) {
				existing.texts.push(text);
				if (contextToken) existing.contextToken = contextToken;
				return { kind: "dispatch", dispatch: this.take(sender, existing) };
			}
		}

		// Standalone text, or image dispatch when batching is disabled.
		return { kind: "dispatch", dispatch: this.build(sender, { text, images, contextToken }, false) };
	}

	/** Flush a sender's pending batch (the flush timer fired with no caption). Returns undefined when none. */
	flush(sender: string): BatchDispatch | undefined {
		const existing = this.batches.get(sender);
		if (!existing) return undefined;
		return this.take(sender, existing);
	}

	/** Whether a sender currently has a buffered batch (tests / diagnostics). */
	hasPending(sender: string): boolean {
		return this.batches.has(sender);
	}

	/** Number of images currently buffered for a sender (0 when none; diagnostics/logging). */
	pendingImageCount(sender: string): number {
		return this.batches.get(sender)?.images.length ?? 0;
	}

	/** Number of senders with a buffered batch (tests / diagnostics). */
	pendingCount(): number {
		return this.batches.size;
	}

	private take(sender: string, batch: PendingBatch): BatchDispatch {
		this.batches.delete(sender);
		return this.build(
			sender,
			{ text: batch.texts.join("\n"), images: batch.images, contextToken: batch.contextToken },
			true,
		);
	}

	private build(sender: string, piece: PendingPiece, isBatch: boolean): BatchDispatch {
		const { text, images, contextToken } = piece;
		// Image-only message: keep a placeholder so greeting/routing/logs still function.
		const safeText = text || "[图片]";
		const imageSig = images.length
			? crypto.createHash("md5").update(Buffer.concat(images.map((i) => i.buffer))).digest("hex").slice(0, 8)
			: "";
		// Short text signature so different captions on the same photo don't collide.
		const textSig = safeText.slice(0, 20);
		const id = isBatch
			? `${sender}:batch:${imageSig}:${textSig}`
			: `${sender}:${safeText}:${imageSig}`;
		return { text: safeText, images, contextToken, id };
	}
}
