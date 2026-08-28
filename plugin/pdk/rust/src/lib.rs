// hwcloud-pdk — Rust PDK for building WASM plugins for hwcloud.
//
// Add to Cargo.toml:
//   [dependencies]
//   hwcloud-pdk = { git = "https://github.com/Cloud-Developer-Department/hwcloud", path = "plugin/pdk/rust" }
//
// Quick start:
//   use hwcloud_pdk::prelude::*;

#![cfg_attr(not(test), no_std)]

extern crate alloc;

pub mod types;
pub mod export;

// host wrappers reference wasm imports ("host" module) that don't exist in
// host-side test builds — gate them out so cargo test can link.
#[cfg(not(test))]
pub mod host;

// ── allocator ──
//
// dlmalloc backed by wasm memory.grow — the std default allocator for
// wasm32-unknown-unknown. Grows linear memory on demand (clamped by the
// host's WithMemoryLimitPages = 512 MiB) instead of a fixed BSS heap.
use core::alloc::{GlobalAlloc, Layout};

// We wrap Dlmalloc ourselves instead of using the `global` feature's
// GlobalDlmalloc because the host→guest ABI frees buffers with only a
// pointer (C-style `free(ptr)`), no Layout. GlobalDlmalloc::dealloc calls
// Dlmalloc::free which runs validate_size(ptr, layout.size()) — an
// assertion that fails when the size doesn't match the allocation. Our
// wrapper's dealloc calls c_free(ptr) directly, which reads the chunk
// size from dlmalloc's own header and needs no Layout.
struct DlmallocWrapper;

unsafe impl GlobalAlloc for DlmallocWrapper {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        (*core::ptr::addr_of_mut!(DLMALLOC)).malloc(layout.size(), layout.align())
    }
    unsafe fn dealloc(&self, ptr: *mut u8, _layout: Layout) {
        (*core::ptr::addr_of_mut!(DLMALLOC)).c_free(ptr)
    }
    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        (*core::ptr::addr_of_mut!(DLMALLOC)).calloc(layout.size(), layout.align())
    }
    unsafe fn realloc(&self, ptr: *mut u8, _layout: Layout, new_size: usize) -> *mut u8 {
        (*core::ptr::addr_of_mut!(DLMALLOC)).c_realloc(ptr, new_size)
    }
}

static mut DLMALLOC: dlmalloc::Dlmalloc = dlmalloc::Dlmalloc::new();

#[cfg_attr(not(test), global_allocator)]
static ALLOC: DlmallocWrapper = DlmallocWrapper;


// ── panic handler ──

// std provides its own panic handler in test builds.
//
// A guest panic must surface to the host as a wazero call error, never
// hang it: observer calls run on a background context that is never
// cancelled, so a `loop {}` here would deadlock the observer worker (and
// every later stage event with it) forever. We report the reason via
// host log_error (stack buffer only — allocating during a panic risks a
// re-entrant second panic, e.g. when the panic IS an out-of-memory),
// then trap.
//
// NOTE: `unreachable!()` would re-enter this handler and spin again —
// only hint::unreachable_unchecked (the wasm `unreachable` instruction)
// actually traps.
#[cfg(not(test))]
#[panic_handler]
fn panic_handler(info: &core::panic::PanicInfo) -> ! {
    let mut buf = [0u8; 512];
    let written = {
        let mut w = StackBuf(&mut buf, 0);
        let _ = core::fmt::write(&mut w, format_args!("plugin panic: {}", info));
        w.1
    };
    if written > 0 {
        let msg = unsafe { core::str::from_utf8_unchecked(&buf[..written]) };
        crate::host::log_error(msg);
    }
    unsafe { core::hint::unreachable_unchecked() }
}

/// Bounded stack writer for the panic message — never touches the heap.
struct StackBuf<'a>(&'a mut [u8], usize); // (buffer, bytes written)

impl core::fmt::Write for StackBuf<'_> {
    fn write_str(&mut self, s: &str) -> core::fmt::Result {
        let b = s.as_bytes();
        if self.1 + b.len() <= self.0.len() {
            self.0[self.1..self.1 + b.len()].copy_from_slice(b);
            self.1 += b.len();
            Ok(())
        } else {
            Err(core::fmt::Error) // truncate — the host log is enough
        }
    }
}

// ── ABI helpers ──

/// Pack (ptr, len) into a single u64 — high 32 = ptr, low 32 = len.
pub fn pk(p: u32, l: u32) -> u64 { ((p as u64) << 32) | (l as u64) }

/// Unpack a u64 into (ptr, len).
pub fn up(u: u64) -> (u32, u32) { ((u >> 32) as u32, (u & 0xFFFF_FFFF) as u32) }

/// Read a string from WASM linear memory at (ptr, len).
pub unsafe fn wasm_str(p: u32, l: u32) -> &'static str {
    if p == 0 && l == 0 { return "" }
    core::str::from_utf8_unchecked(core::slice::from_raw_parts(p as *const u8, l as usize))
}

/// Allocate memory in the WASM linear heap. Export this from your .wasm.
/// Returns 0 (null) when the heap is exhausted — callers must check.
pub fn sdk_alloc(size: u32) -> u32 {
    let layout = Layout::array::<u8>(size as usize).unwrap();
    unsafe { GlobalAlloc::alloc(&ALLOC, layout) as u32 }
}

/// Return a heap allocation to the allocator. Exported as `dealloc` (see
/// the export! macro) so the host can free the buffers it allocated (call
/// inputs) and the packed results it read. Null pointers are ignored
/// (dlmalloc's dealloc treats null as UB; the host's FreeBytes also filters
/// ptr==0, so this guard is belt-and-suspenders).
pub fn sdk_dealloc(ptr: u32) {
    if ptr == 0 {
        return;
    }
    unsafe { GlobalAlloc::dealloc(&ALLOC, ptr as *mut u8, Layout::new::<u8>()) }
}

/// Pack static JSON for no-arg exports (e.g. metadata).
pub fn sdk_meta(json: &str) -> u64 { pk(json.as_ptr() as u32, json.len() as u32) }

/// Allocate + copy data into guest memory, return packed (ptr, len).
/// Returns 0 on allocation failure — never writes through null.
pub fn sdk_return(data: &[u8]) -> u64 {
    if data.is_empty() { return 0 }
    let p = sdk_alloc(data.len() as u32);
    if p == 0 { return 0 } // heap exhausted — report empty, don't write address 0
    unsafe { core::slice::from_raw_parts_mut(p as *mut u8, data.len()).copy_from_slice(data) }
    pk(p, data.len() as u32)
}

/// sdk_return on a serializable value.
pub fn sdk_return_json(v: &impl serde::Serialize) -> u64 {
    match serde_json::to_string(v) {
        Ok(s) => sdk_return(s.as_bytes()),
        Err(_) => 0,
    }
}

/// Read bytes from guest memory at packed (ptr, len).
pub fn read_input(packed: u64) -> &'static [u8] {
    let (p, l) = up(packed);
    if p == 0 && l == 0 { return &[] }
    unsafe { core::slice::from_raw_parts(p as *const u8, l as usize) }
}

/// Deserialize guest memory at packed (ptr, len) into T.
/// Returns default value on parse failure.
pub fn read_input_json<T: serde::de::DeserializeOwned + Default + 'static>(packed: u64) -> T {
    serde_json::from_slice(read_input(packed)).unwrap_or_default()
}

/// Read a string from guest memory at (ptr, len). Safe wrapper around wasm_str.
pub fn read_input_str(ptr: u32, len: u32) -> &'static str {
    unsafe { wasm_str(ptr, len) }
}

/// Dispatch a CLI command by name. Used by cli:commands plugins in their
/// hand-written `run_<name>` exports (one line per command).
pub fn dispatch_command<T: export::Plugin>(ptr: u32, len: u32, name: &str) -> u64 {
    let input: types::CommandInput = read_input_json(pk(ptr, len));
    match T::run_command(name, &input) {
        Ok(s) => sdk_return(s.as_bytes()),
        Err(e) => sdk_return(e.as_bytes()),
    }
}

// ── prelude ──

pub mod prelude {
    pub use alloc::string::String;
    pub use alloc::collections::BTreeMap;
    pub use alloc::vec;
    pub use alloc::vec::Vec;
    pub use alloc::format;
    pub use serde_json;
    #[cfg(not(test))]
    pub use crate::host;
    pub use crate::types::*;
    pub use crate::{sdk_alloc, sdk_dealloc, sdk_meta, sdk_return, sdk_return_json, pk, up, wasm_str};
}

// ── allocator tests ──
//
// dlmalloc is a black box — tests assert observable contract properties
// (non-null, distinct, writeable, no-OOM under churn) via the public
// sdk_alloc/sdk_dealloc ABI, not internal free-list structure.

#[cfg(test)]
mod allocator_tests {
    use super::*;
    use core::alloc::{GlobalAlloc, Layout};

    // dlmalloc is a black box — tests assert observable contract properties
    // (non-null, distinct, writeable, no-OOM under churn) via GlobalAlloc
    // directly, NOT via sdk_alloc/sdk_dealloc. The sdk_* functions use u32
    // pointers (wasm32 ABI); on native 64-bit test builds they truncate
    // dlmalloc's 64-bit mmap addresses and segfault. On wasm32 the u32 ABI
    // is correct, so sdk_alloc/sdk_dealloc are exercised by the example
    // plugins' integration tests instead.
    //
    // NOTE: double-free is NOT tested — dlmalloc treats it as UB. The host
    // ABI (CallWithInput) frees each buffer exactly once; sdk_dealloc keeps
    // a null guard only.

    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn fresh<T>(f: impl FnOnce() -> T) -> T {
        let _g = LOCK.lock().unwrap_or_else(|e| e.into_inner());
        f()
    }

    fn alloc(size: usize, align: usize) -> *mut u8 {
        let layout = Layout::from_size_align(size, align).unwrap();
        unsafe { GlobalAlloc::alloc(&ALLOC, layout) }
    }

    fn dealloc(p: *mut u8, size: usize, align: usize) {
        let layout = Layout::from_size_align(size, align).unwrap();
        unsafe { GlobalAlloc::dealloc(&ALLOC, p, layout) }
    }

    #[test]
    fn alloc_returns_nonnull() {
        fresh(|| {
            assert!(!alloc(1, 1).is_null(), "1-byte alloc");
            assert!(!alloc(100, 1).is_null(), "100-byte alloc");
            assert!(!alloc(0, 1).is_null(), "zero-size alloc must be non-null");
            assert!(!alloc(1 << 20, 1).is_null(), "1 MiB alloc");
        });
    }

    #[test]
    fn alloc_distinct_ptrs() {
        fresh(|| {
            let a = alloc(64, 1);
            let b = alloc(64, 1);
            assert!(!a.is_null() && !b.is_null(), "both must succeed");
            assert_ne!(a, b, "distinct allocs must return distinct pointers");
        });
    }

    #[test]
    fn alloc_dealloc_no_oom() {
        fresh(|| {
            // 1000 alloc→dealloc cycles must not exhaust memory.
            // dlmalloc reuses freed blocks; if it leaked, this would OOM.
            for _ in 0..1000 {
                let p = alloc(8192, 1);
                assert!(!p.is_null(), "alloc must not fail under churn");
                dealloc(p, 8192, 1);
            }
        });
    }

    #[test]
    fn dealloc_null_safe() {
        fresh(|| {
            // GlobalAlloc::dealloc with null is a no-op for dlmalloc
            // (c_free checks is_null). Our sdk_dealloc also guards ptr==0.
            dealloc(std::ptr::null_mut(), 32, 1);
            let p = alloc(32, 1);
            assert!(!p.is_null(), "alloc after dealloc(null) must succeed");
            dealloc(p, 32, 1);
        });
    }

    #[test]
    fn large_alloc_succeeds() {
        fresh(|| {
            // 20 MiB allocation. On wasm32 this exceeds the initial ~1 MiB
            // and forces memory.grow; on native it mmap's a fresh region.
            // Either way, it must succeed (we have no 4 MiB BSS cap now).
            let p = alloc(20 * 1024 * 1024, 1);
            assert!(!p.is_null(), "20 MiB alloc must succeed");
            dealloc(p, 20 * 1024 * 1024, 1);
        });
    }

    #[test]
    fn write_through_alloc() {
        fresh(|| {
            let size = 256;
            let p = alloc(size, 1);
            assert!(!p.is_null());
            unsafe {
                for i in 0..size {
                    *p.add(i) = (i & 0xFF) as u8;
                }
                for i in 0..size {
                    assert_eq!(*p.add(i), (i & 0xFF) as u8, "memory must be writable and readable");
                }
            }
            dealloc(p, size, 1);
        });
    }

    #[test]
    fn alignment_is_honoured() {
        fresh(|| {
            // dlmalloc's GlobalAlloc::alloc respects Layout::align.
            let p = alloc(24, 16);
            assert!(!p.is_null(), "16-aligned alloc must succeed");
            assert_eq!(p as usize % 16, 0, "payload must be 16-aligned");
            dealloc(p, 24, 16);

            let q = alloc(9, 8);
            assert!(!q.is_null());
            assert_eq!(q as usize % 8, 0, "payload must be 8-aligned");
            dealloc(q, 9, 8);
        });
    }

    #[test]
    fn many_allocs_then_free_all() {
        fresh(|| {
            // Allocate many blocks of varying sizes, then free all.
            // Tests that dlmalloc doesn't corrupt its metadata under
            // realistic allocation patterns.
            let mut ptrs = Vec::new();
            for i in 1..=100 {
                let p = alloc(i * 64, 1);
                assert!(!p.is_null(), "alloc {} failed", i);
                ptrs.push((p, i * 64));
            }
            for (p, s) in &ptrs {
                dealloc(*p, *s, 1);
            }
            // After freeing everything, a large alloc must still work.
            let big = alloc(4 * 1024 * 1024, 1);
            assert!(!big.is_null(), "4 MiB alloc after mass free must succeed");
            dealloc(big, 4 * 1024 * 1024, 1);
        });
    }

    #[test]
    fn sdk_dealloc_null_is_safe() {
        fresh(|| {
            // sdk_dealloc(0) must be a no-op — the guard prevents calling
            // dlmalloc with a null pointer (which c_free handles, but the
            // guard is belt-and-suspenders for the host ABI).
            sdk_dealloc(0);
        });
    }
}
