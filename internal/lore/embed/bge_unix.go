// BGEEmbedder.Embed: the ORT-heavy inference path. Separated from bge.go
// so the tensor-allocation boilerplate is easy to review without the
// lifecycle noise. Build tag matches bge.go (unix only).

//go:build unix

package embed

import (
	"context"
	"fmt"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

// Embed tokenizes text, runs one forward pass through the ONNX model,
// CLS-pools the last hidden state to Dim floats, and L2-normalizes.
// Returns a fresh slice (no aliasing of internal ORT buffers).
//
// Context cancellation is honored on the Go side before and between
// allocations; ORT's Run itself is uncancellable, so a Cancel during an
// already-running forward pass waits for that pass to finish.
func (e *BGEEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids, mask, typeIDs := e.tokenizer.Encode(text, e.maxLen)
	seqLen := int64(len(ids))

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rt == nil || e.rt.session == nil {
		return nil, ErrEmbedderClosed
	}

	idsT, err := ort.NewTensorValue(e.rt.rt, ids, []int64{1, seqLen})
	if err != nil {
		return nil, fmt.Errorf("embed: input_ids tensor: %w", err)
	}
	defer idsT.Close()
	maskT, err := ort.NewTensorValue(e.rt.rt, mask, []int64{1, seqLen})
	if err != nil {
		return nil, fmt.Errorf("embed: attention_mask tensor: %w", err)
	}
	defer maskT.Close()
	typeT, err := ort.NewTensorValue(e.rt.rt, typeIDs, []int64{1, seqLen})
	if err != nil {
		return nil, fmt.Errorf("embed: token_type_ids tensor: %w", err)
	}
	defer typeT.Close()

	inputs := map[string]*ort.Value{
		"input_ids":      idsT,
		"attention_mask": maskT,
		"token_type_ids": typeT,
	}
	outputs, err := e.rt.session.Run(ctx, inputs, ort.WithOutputNames("last_hidden_state"))
	if err != nil {
		return nil, fmt.Errorf("embed: ort Run: %w", err)
	}
	// F5: the output map does not auto-close values. Explicit Close in a
	// defer so long inscribe loops do not balloon RSS waiting on finalizers.
	defer func() {
		for _, v := range outputs {
			v.Close()
		}
	}()

	lhs := outputs["last_hidden_state"]
	data, shape, err := ort.GetTensorData[float32](lhs)
	if err != nil {
		return nil, fmt.Errorf("embed: GetTensorData: %w", err)
	}
	if len(shape) != 3 || shape[0] != 1 || shape[2] != int64(e.dim) {
		return nil, fmt.Errorf("%w: %v", ErrUnexpectedOutputShape, shape)
	}
	// CLS token is position 0 in the sequence dimension. Slice the first
	// Dim floats (a fresh copy so the returned buffer does not alias the
	// ORT tensor, which is freed by the deferred Close above).
	cls := make([]float32, e.dim)
	copy(cls, data[:e.dim])
	l2Normalize(cls)
	return cls, nil
}
