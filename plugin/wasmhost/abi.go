package wasmhost

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// Pack encodes (ptr, length) into a single u64 — high 32 bits = ptr, low 32 bits = len.
// This is the WASM ABI convention used by all plugin exports.
func Pack(ptr, length uint32) uint64 {
	return (uint64(ptr) << 32) | uint64(length)
}

// Unpack decodes a u64 into (ptr, length).
func Unpack(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed & 0xFFFF_FFFF)
}

// ReadPacked reads bytes from guest memory at the (ptr, length) location
// encoded as a packed u64. Returns nil if the read fails or the pointer is
// zero.
func ReadPacked(mod api.Module, packed uint64) []byte {
	ptr, length := Unpack(packed)
	if ptr == 0 && length == 0 {
		return nil
	}
	data, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil
	}
	out := make([]byte, length)
	copy(out, data)
	return out
}

// ReadString reads N bytes from guest memory at ptr, returning a Go string.
func ReadString(mod api.Module, ptr, length uint32) string {
	if ptr == 0 && length == 0 {
		return ""
	}
	data, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return ""
	}
	return string(data)
}

// WriteString writes data into guest memory via the guest's alloc function.
// Caller provides the context for the alloc call.
//
// Allocation failure (guest heap exhausted) returns 0 — the caller sees
// an empty result instead of a (0, len) pair that would make the guest
// read garbage at address 0 (serde then panics on the bogus length, or
// worse, silently parses heap bytes). The empty result is the guest's
// signal that something went wrong; the host-side callers that produce
// large payloads (exec_command) additionally reject oversized responses
// with a specific error before this point.
func WriteString(ctx context.Context, mod api.Module, data []byte) uint64 {
	if len(data) == 0 {
		return 0
	}
	allocFn := mod.ExportedFunction("alloc")
	if allocFn == nil {
		return 0
	}
	results, err := allocFn.Call(ctx, uint64(len(data)))
	if err != nil || len(results) == 0 || results[0] == 0 {
		return 0 // alloc returned null — guest heap exhausted
	}
	ptr := uint32(results[0])
	if !mod.Memory().Write(ptr, data) {
		return 0 // out of bounds — never hand the guest a bogus (ptr, len)
	}
	return Pack(ptr, uint32(len(data)))
}

// FreeBytes returns a guest buffer — previously handed out by the guest's
// alloc export — back to the guest heap via the guest's dealloc export.
//
// No-op when the guest lacks dealloc (older .wasm binaries: the buffer
// leaks exactly as it did before, and mixed-version deployments degrade
// gracefully) or when ptr is 0. Best-effort: a failing dealloc must never
// surface as an error in the calling code.
func FreeBytes(ctx context.Context, mod api.Module, ptr uint32) {
	if ptr == 0 {
		return
	}
	dealloc := mod.ExportedFunction("dealloc")
	if dealloc == nil {
		return
	}
	dealloc.Call(ctx, uint64(ptr))
}

// FreePacked is FreeBytes for a packed (ptr, len) result.
func FreePacked(ctx context.Context, mod api.Module, packed uint64) {
	ptr, _ := Unpack(packed)
	FreeBytes(ctx, mod, ptr)
}

// CallWithInput calls a guest export that takes packed (ptr, len) input and
// returns a packed (ptr, len) result — the ABI convention shared by the CLI
// and agent plugin loaders (previously duplicated in both).
//
// Empty input calls the export with no arguments. A nil return means the
// export returned an empty result ((0, 0)); callers decide whether that is
// an error.
//
// Buffer lifecycle: for non-empty input, the input buffer is allocated
// through the guest's alloc export, and the export's result buffer is
// allocated by the guest itself (sdk_return) — both are returned to the
// guest heap before this returns. Results are transient by convention:
// the host reads them once, then frees.
func CallWithInput(ctx context.Context, mod api.Module, fnName string, input []byte) ([]byte, error) {
	fn := mod.ExportedFunction(fnName)
	if fn == nil {
		return nil, fmt.Errorf("export %q not found", fnName)
	}

	var results []uint64
	var inPtr uint32
	if len(input) == 0 {
		var err error
		results, err = fn.Call(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fnName, err)
		}
	} else {
		allocFn := mod.ExportedFunction("alloc")
		if allocFn == nil {
			return nil, fmt.Errorf("export %q: alloc not exported", fnName)
		}
		allocRes, err := allocFn.Call(ctx, uint64(len(input)))
		if err != nil || len(allocRes) == 0 {
			return nil, fmt.Errorf("%s: alloc: %w", fnName, err)
		}
		inPtr = uint32(allocRes[0])
		if !mod.Memory().Write(inPtr, input) {
			FreeBytes(ctx, mod, inPtr)
			return nil, fmt.Errorf("%s: write out of bounds", fnName)
		}
		results, err = fn.Call(ctx, uint64(inPtr), uint64(len(input)))
		if err != nil {
			FreeBytes(ctx, mod, inPtr)
			return nil, fmt.Errorf("%s: %w", fnName, err)
		}
	}
	if len(results) == 0 {
		FreeBytes(ctx, mod, inPtr)
		return nil, fmt.Errorf("%s: no result", fnName)
	}
	data := ReadPacked(mod, results[0])
	FreeBytes(ctx, mod, inPtr)
	FreePacked(ctx, mod, results[0])
	return data, nil
}
