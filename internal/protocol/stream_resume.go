package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ForwardStreamResumable relays one upstream SSE stream to the client and,
// when the stream dies mid-flight after visible text was delivered, asks
// resumer for a continuation segment and splices its frames into the same
// downstream stream. With resumer == nil or maxResumes <= 0 it behaves
// exactly like the historical ForwardStream.
func ForwardStreamResumable(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string, resumer StreamResumer, maxResumes int) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	observer := newStreamUsageObserver(protocol)
	observer.trackContent = resumer != nil
	// Per-segment accounting: the continuation request replays the partial
	// as prompt, so its reported Input would inflate the true first-segment
	// cost and its Output alone would hide the delivered first-segment
	// tokens. Input/Cached come from the first segment only; Output and
	// Reasoning accumulate across segments (review: "wrong usage after
	// continuation").
	var total Usage
	first := reader
	resumes := 0
	for {
		_, copyErr := io.Copy(&sseFlushWriter{writer: w, flusher: flusher}, io.TeeReader(reader, observer))
		// Snapshot the torn tail before Finish clears the frame buffer.
		tornTail := observer.Pending()
		segUsage := observer.Finish()
		if resumes == 0 {
			total.Input = segUsage.Input
			total.Cached = segUsage.Cached
			total.CacheCreation = segUsage.CacheCreation
		}
		total.Output += segUsage.Output
		total.Reasoning += segUsage.Reasoning
		total.Total = max(total.Total, total.Input+total.Output)
		usage := total
		if observer.ErrorTermination() {
			reportResumeSkipped(resumer, "upstream_error_event", observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
			closeCurrentIfNotFirst(reader, first)
			if copyErr != nil {
				return usage, observer.Reported(), copyErr
			}
			return usage, observer.Reported(), errStreamUpstreamFailure
		}
		if observer.NormalTermination() && observer.ParseError() == nil {
			closeCurrentIfNotFirst(reader, first)
			return usage, observer.Reported(), copyErr
		}
		if ClientCanceled(ctx, copyErr) {
			// A client that walked away must see no synthetic error and no
			// resume attempt; the bytes would go nowhere.
			reportResumeSkipped(resumer, "client_canceled", observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
			closeCurrentIfNotFirst(reader, first)
			if copyErr == nil {
				copyErr = ctx.Err()
			}
			return usage, observer.Reported(), copyErr
		}
		// Unexpected termination — the only resume window.
		if resumer != nil && resumes < maxResumes && !observer.ToolSeen() && !ClientCanceled(ctx, nil) {
			// Rune counting runs only when a resumer is mounted; the
			// disabled path pays nothing (review: "disabled path not free").
			canResumeText := observer.TextRunes() > 0
			canResumeReasoning := !canResumeText && observer.ReasoningRunes() > 0
			if canResumeText || canResumeReasoning {
				if resumeObserver, ok := resumer.(StreamResumeObserver); ok {
					resumeObserver.ResumeEligible(observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
				}
				tail := observer.TailRunes(200)
				if terminator := missingRecordTerminator(tornTail); len(terminator) > 0 {
					// The dead stream stopped mid-frame and those bytes are
					// already downstream; close the torn record now so neither
					// the pings nor the continuation's first frame join it and
					// get dropped by the client parser.
					if _, err := w.Write(terminator); err != nil {
						return usage, observer.Reported(), err
					}
					flusher.Flush()
				}
				next, resumeErr := resumeWithPing(ctx, w, flusher, resumer, observer.Text(), observer.Reasoning())
				if resumeErr == nil {
					closeCurrentIfNotFirst(reader, first)
					// Clear the dead segment's parse fault so a clean
					// continuation is not misclassified as failed; a systemic
					// fault re-surfaces on the new segment by itself.
					observer.parseErr = nil
					filtered := newContinuationFrameFilter(next)
					if canResumeText {
						reader = newLCPTrimReader(filtered, tail)
					} else {
						reader = filtered
					}
					resumes++
					continue
				}
				if ClientCanceled(ctx, resumeErr) {
					closeCurrentIfNotFirst(reader, first)
					return usage, observer.Reported(), resumeErr
				}
				// The resume failed; fall through and emit the in-band error
				// with the ORIGINAL cause, exactly as the no-resume path does.
			} else {
				reportResumeSkipped(resumer, "no_visible_text", observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
			}
		} else if resumer != nil {
			reason := "resume_limit"
			switch {
			case resumes < maxResumes && observer.ToolSeen():
				reason = "tool_seen"
			case resumes < maxResumes && ClientCanceled(ctx, nil):
				reason = "client_canceled"
			}
			reportResumeSkipped(resumer, reason, observer.visibleBytes, observer.reasoningBytes, observer.toolSeen)
		}
		cause := observer.ParseError()
		if copyErr != nil {
			cause = copyErr
		}
		if cause == nil {
			cause = errSSEUnexpectedEOF
		}
		emitter := newBridgeStreamEmitter(w, flusher, protocol, model)
		if err := emitUnexpectedStreamError(emitter, cause); err != nil {
			closeCurrentIfNotFirst(reader, first)
			return usage, observer.Reported(), err
		}
		closeCurrentIfNotFirst(reader, first)
		return usage, observer.Reported(), cause
	}
}

// closeCurrentIfNotFirst closes the active segment reader once it is done.
// The first reader is owned by the caller and is never closed here; every
// resumed segment reader owns its upstream response body and must be
// released to cancel its attempt context.
func closeCurrentIfNotFirst(reader io.Reader, first io.Reader) {
	if reader == first {
		return
	}
	if closer, ok := reader.(io.Closer); ok {
		_ = closer.Close()
	}
}

func reportResumeSkipped(resumer StreamResumer, reason string, visibleBytes, reasoningBytes int, toolSeen bool) {
	if observer, ok := resumer.(StreamResumeObserver); ok {
		observer.ResumeSkipped(reason, visibleBytes, reasoningBytes, toolSeen)
	}
}

// missingRecordTerminator returns the bytes needed to close an SSE record
// left open by a torn frame: both newlines when it ended mid-line, the
// missing one when it stopped after a single newline, nothing when the
// record boundary is already complete.
func missingRecordTerminator(tail []byte) []byte {
	if len(tail) == 0 {
		return nil
	}
	newlines := 0
	for i := len(tail) - 1; i >= 0 && (tail[i] == '\n' || tail[i] == '\r'); i-- {
		if tail[i] == '\n' {
			newlines++
		}
	}
	switch newlines {
	case 0:
		return []byte("\n\n")
	case 1:
		return []byte("\n")
	default:
		return nil
	}
}

// resumeWithPing fetches the continuation segment while writing an SSE
// comment every 10s so intermediaries do not time the connection out during
// the fetch. The goroutine is fully stopped before returning, so no ping can
// be written after the next frames start flowing.
func resumeWithPing(ctx context.Context, w io.Writer, flusher http.Flusher, resumer StreamResumer, partial string, reasoning string) (io.Reader, error) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// A ping write error means the client is gone; the caller's
				// context check decides what to do, not this ticker.
				_, _ = io.WriteString(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}()
	reader, err := resumer.ResumeSegment(partial, reasoning)
	close(stop)
	<-done
	return reader, err
}

const lcpMaxOverlapRunes = 200

const lcpMinOverlapRunes = 4

type lcpTrimReader struct {
	src     io.Reader
	tail    string
	pending []byte
	buf     []byte
	tmp     []byte
	done    bool
	srcEOF  bool
	srcErr  error

	frames  [][]byte
	accText []rune
}

// newLCPTrimReader returns a frame-aware reader over a continuation segment.
// The leading frames are buffered until enough content has accumulated to
// decide the overlap (or a frame-count/terminator cap is hit); the overlap is
// then trimmed across those frames and every later byte passes through
// unchanged. Parse/rewrite failures pass buffered frames through unchanged
// (fail-open): dedup must never break the stream.
func newLCPTrimReader(src io.Reader, tail string) io.Reader {
	return &lcpTrimReader{src: src, tail: tail, tmp: make([]byte, 32<<10)}
}

func (reader *lcpTrimReader) Read(p []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.done {
			// After the scan ends, buffered remainder is stream bytes that
			// must still reach the client verbatim before passthrough.
			if len(reader.buf) > 0 {
				reader.pending = reader.buf
				reader.buf = nil
				continue
			}
			if reader.srcEOF {
				return 0, reader.srcErr
			}
			return reader.src.Read(p)
		}
		if index, width := nextSSEBoundary(reader.buf); index >= 0 {
			frame := make([]byte, index+width)
			copy(frame, reader.buf[:index+width])
			reader.buf = reader.buf[index+width:]
			reader.collect(frame)
			continue
		}
		if reader.srcEOF {
			// Residual bytes without a final boundary are served as-is.
			reader.pending = reader.buf
			reader.buf = nil
			reader.done = true
			continue
		}
		n, err := reader.src.Read(reader.tmp)
		if n > 0 {
			reader.buf = append(reader.buf, reader.tmp[:n]...)
		}
		if err != nil {
			reader.srcEOF = true
			if err != io.EOF {
				reader.srcErr = err
			} else {
				reader.srcErr = io.EOF
			}
		}
	}
	n := copy(p, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// Close releases the underlying segment reader, cancelling its attempt
// context through the gateway's attemptBody wrapper.
func (reader *lcpTrimReader) Close() error {
	if closer, ok := reader.src.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// collect buffers one complete frame during the overlap scan and decides
// when enough content has been seen to determine the trim.
func (reader *lcpTrimReader) collect(frame []byte) {
	if reader.done {
		reader.pending = append(reader.pending, frame...)
		return
	}
	data, ok := sseFrameData(frame)
	if !ok {
		// A comment or event-only frame carries no JSON; emit and keep
		// scanning.
		reader.pending = append(reader.pending, frame...)
		return
	}
	if data == "[DONE]" {
		reader.flushDecision()
		reader.pending = append(reader.pending, frame...)
		reader.done = true
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		// Unparseable data: fail open with the buffered frames unchanged.
		reader.flushUnchanged()
		reader.pending = append(reader.pending, frame...)
		reader.done = true
		return
	}
	reader.frames = append(reader.frames, frame)
	if _, text, ok := firstChatDelta(payload); ok {
		reader.accText = append(reader.accText, []rune(text)...)
	}
	if len(reader.accText) >= lcpMaxOverlapRunes ||
		(len(reader.tail) > 0 && len(reader.accText) >= len(reader.tail)) {
		reader.flushDecision()
		reader.done = true
	}
}

func (reader *lcpTrimReader) flushDecision() {
	if len(reader.frames) == 0 {
		return
	}
	k := longestOverlap(string(reader.accText), reader.tail, lcpMaxOverlapRunes)
	reader.pending = append(reader.pending, reader.rewriteFrames(k)...)
	reader.frames = nil
	reader.accText = nil
}

func (reader *lcpTrimReader) flushUnchanged() {
	for _, frame := range reader.frames {
		reader.pending = append(reader.pending, frame...)
	}
	reader.frames = nil
	reader.accText = nil
}

// rewriteFrames consumes k runes of overlap from the front of the buffered
// content frames in order; fully consumed frames keep their envelope with an
// empty content field so frame identity (id/created) is preserved.
func (reader *lcpTrimReader) rewriteFrames(k int) []byte {
	var out []byte
	remaining := k
	for _, frame := range reader.frames {
		if remaining == 0 {
			out = append(out, frame...)
			continue
		}
		data, ok := sseFrameData(frame)
		if !ok || data == "[DONE]" {
			out = append(out, frame...)
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			out = append(out, frame...)
			continue
		}
		delta, text, ok := firstChatDelta(payload)
		if !ok {
			out = append(out, frame...)
			continue
		}
		runes := []rune(text)
		if remaining >= len(runes) {
			remaining -= len(runes)
			delta["content"] = ""
		} else {
			delta["content"] = string(runes[remaining:])
			remaining = 0
		}
		rewritten, err := json.Marshal(payload)
		if err != nil {
			out = append(out, frame...)
			continue
		}
		out = append(out, []byte("data: "+string(rewritten)+"\n\n")...)
	}
	return out
}

// continuationFrameFilter drops the frames a fresh continuation segment
// re-emits that the client already saw from the first segment: the role head,
// reasoning deltas and empty-content noise frames. Parse failures pass
// through unchanged (fail-open).
type continuationFrameFilter struct {
	src     io.Reader
	pending []byte
	buf     []byte
	tmp     []byte
	done    bool
	srcEOF  bool
	srcErr  error
}

func newContinuationFrameFilter(src io.Reader) io.Reader {
	return &continuationFrameFilter{src: src, tmp: make([]byte, 32<<10)}
}

func (reader *continuationFrameFilter) Read(p []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.done {
			if len(reader.buf) > 0 {
				reader.pending = reader.buf
				reader.buf = nil
				continue
			}
			if reader.srcEOF {
				return 0, reader.srcErr
			}
			return reader.src.Read(p)
		}
		if index, width := nextSSEBoundary(reader.buf); index >= 0 {
			frame := make([]byte, index+width)
			copy(frame, reader.buf[:index+width])
			reader.buf = reader.buf[index+width:]
			if !dropContinuationFrame(frame) {
				reader.pending = append(reader.pending, frame...)
			}
			continue
		}
		if reader.srcEOF {
			reader.pending = reader.buf
			reader.buf = nil
			reader.done = true
			continue
		}
		n, err := reader.src.Read(reader.tmp)
		if n > 0 {
			reader.buf = append(reader.buf, reader.tmp[:n]...)
		}
		if err != nil {
			reader.srcEOF = true
			if err != io.EOF {
				reader.srcErr = err
			} else {
				reader.srcErr = io.EOF
			}
		}
	}
	n := copy(p, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

func (reader *continuationFrameFilter) Close() error {
	if closer, ok := reader.src.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func dropContinuationFrame(frame []byte) bool {
	data, ok := sseFrameData(frame)
	if !ok || data == "[DONE]" {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return false
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return false
	}
	if finish, _ := choice["finish_reason"].(string); finish != "" {
		return false
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return false
	}
	if content, _ := delta["content"].(string); content != "" {
		return false
	}
	return true
}

// sseFrameData joins a frame's data: lines via readSSE rules; false means no
// data payload (comments, event-only frames).
func sseFrameData(frame []byte) (string, bool) {
	var joined string
	found := false
	_ = readSSE(bytes.NewReader(frame), func(eventName, value string) error {
		if found {
			joined += "\n" + value
		} else {
			joined = value
			found = true
		}
		return nil
	})
	return joined, found
}

func firstChatDelta(payload map[string]any) (map[string]any, string, bool) {
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, "", false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, "", false
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return nil, "", false
	}
	return delta, content, true
}

// longestOverlap returns the largest k in [minOverlap, min(runeLen(text),
// runeLen(tail), maxK)] whose text prefix equals the tail suffix. Comparison
// is rune-based so multi-byte characters are never split.
func longestOverlap(text, tail string, maxK int) int {
	prefix := []rune(text)
	if len(prefix) > maxK {
		prefix = prefix[:maxK]
	}
	suffix := []rune(tail)
	if len(suffix) > maxK {
		suffix = suffix[len(suffix)-maxK:]
	}
	limit := min(len(prefix), len(suffix))
	for k := limit; k >= lcpMinOverlapRunes; k-- {
		if string(prefix[:k]) == string(suffix[len(suffix)-k:]) {
			return k
		}
	}
	return 0
}
