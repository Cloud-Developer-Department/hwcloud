use alloc::string::String;
// Host API — WASM imports from the "host" module.
//
// All host functions that can fail return packed JSON with an "error" field.
// Each wrapper here calls the raw FFI, reads the returned bytes via up+wasm_str,
// deserializes via serde_json, and checks the error field.

use crate::up;
use crate::{sdk_dealloc};
use crate::types::*;

mod ffi {
    #[link(wasm_import_module = "host")]
    extern "C" {
        pub fn keyring_get(svc_p: u32, svc_l: u32, key_p: u32, key_l: u32) -> u64;
        pub fn keyring_set(svc_p: u32, svc_l: u32, key_p: u32, key_l: u32, val_p: u32, val_l: u32) -> u64;
        pub fn keyring_delete(svc_p: u32, svc_l: u32, key_p: u32, key_l: u32) -> u64;
        pub fn exec_command(json_p: u32, json_l: u32) -> u64;
        pub fn http_request(
            method_p: u32, method_l: u32, url_p: u32, url_l: u32,
            headers_p: u32, headers_l: u32, body_p: u32, body_l: u32,
        ) -> u64;
        pub fn env_get(key_p: u32, key_l: u32) -> u64;
        pub fn env_set(key_p: u32, key_l: u32, val_p: u32, val_l: u32) -> u64;
        pub fn env_unset(key_p: u32, key_l: u32) -> u64;
        pub fn env_list() -> u64;
        pub fn fs_read(path_p: u32, path_l: u32) -> u64;
        pub fn fs_write(path_p: u32, path_l: u32, data_p: u32, data_l: u32) -> u64;
        pub fn fs_readdir(path_p: u32, path_l: u32) -> u64;
        pub fn file_md5(path_p: u32, path_l: u32) -> u64;
        pub fn directory_md5(path_p: u32, path_l: u32) -> u64;
        pub fn log_info(msg_p: u32, msg_l: u32);
        pub fn log_warn(msg_p: u32, msg_l: u32);
        pub fn log_error(msg_p: u32, msg_l: u32);
        pub fn utc_now() -> u64;

        pub fn runtime_session_id() -> u64;
        pub fn runtime_user_id() -> u64;
        pub fn runtime_turn_count() -> u64;
        pub fn runtime_model_id() -> u64;
        pub fn runtime_provider() -> u64;
        pub fn runtime_get_metadata(key_p: u32, key_l: u32) -> u64;
        pub fn runtime_set_metadata(key_p: u32, key_l: u32, val_p: u32, val_l: u32) -> u64;
        pub fn runtime_set_model_config(val_p: u32, val_l: u32) -> u64;
        pub fn runtime_set_embedding_config(val_p: u32, val_l: u32) -> u64;
        pub fn runtime_set_system_prompts(val_p: u32, val_l: u32) -> u64;
        pub fn runtime_set_max_turns(val_p: u32, val_l: u32) -> u64;
    }
}

// ── keyring ──

pub fn keyring_get(service: &str, key: &str) -> Result<String, String> {
    let packed = unsafe { ffi::keyring_get(
        service.as_ptr() as u32, service.len() as u32,
        key.as_ptr() as u32, key.len() as u32,
    )};
    let r: KeyringResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r.value) }
}

pub fn keyring_set(service: &str, key: &str, val: &str) -> Result<(), String> {
    let packed = unsafe { ffi::keyring_set(
        service.as_ptr() as u32, service.len() as u32,
        key.as_ptr() as u32, key.len() as u32,
        val.as_ptr() as u32, val.len() as u32,
    )};
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

pub fn keyring_delete(service: &str, key: &str) -> Result<(), String> {
    let packed = unsafe { ffi::keyring_delete(
        service.as_ptr() as u32, service.len() as u32,
        key.as_ptr() as u32, key.len() as u32,
    )};
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

// ── Exec ──

/// Run a command as a child process. The platform is opaque to the
/// guest: `cmd` is a program name (host PATH lookup) or an explicit
/// path, and `args` is argv after the program — no shell syntax.
///
/// `env` overlays variables on the host process environment (inherited
/// unless overridden). When `env_replace` is true, `env` is the *complete*
/// environment — the host process environment is NOT merged, so host
/// secrets (API keys, tokens) do not leak to the child. `timeout_ms`
/// defaults to 120_000 and is clamped by the host to 10 minutes. `cwd`
/// defaults to the host process cwd.
///
/// A non-zero `exit_code` is a business result, NOT an error — the
/// error string is set only when the command could not run at all,
/// timed out, or exceeded the host's output cap.
pub fn exec_command(
    cmd: &str,
    args: &[&str],
    cwd: Option<&str>,
    env: Option<&alloc::collections::BTreeMap<String, String>>,
    env_replace: bool,
    timeout_ms: Option<u64>,
) -> Result<ExecResult, String> {
    let payload = serde_json::json!({
        "cmd": cmd,
        "args": args,
        "cwd": cwd,
        "env": env,
        "env_replace": env_replace,
        "timeout_ms": timeout_ms,
    });
    let s = serde_json::to_string(&payload)
        .map_err(|e| alloc::format!("exec: serialize: {}", e))?;
    let packed = unsafe { ffi::exec_command(s.as_ptr() as u32, s.len() as u32) };
    let r: ExecResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r) }
}

// ── Environment (host process env) ──

/// Read a host process environment variable.
pub fn env_get(key: &str) -> Result<String, String> {
    let packed = unsafe { ffi::env_get(key.as_ptr() as u32, key.len() as u32) };
    let r: KeyringResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r.value) }
}

/// Set a host process environment variable. Visible to every plugin and
/// to the host itself — not an isolated per-plugin view.
pub fn env_set(key: &str, val: &str) -> Result<(), String> {
    let packed = unsafe { ffi::env_set(
        key.as_ptr() as u32, key.len() as u32,
        val.as_ptr() as u32, val.len() as u32,
    )};
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

/// Remove a host process environment variable.
pub fn env_unset(key: &str) -> Result<(), String> {
    let packed = unsafe { ffi::env_unset(key.as_ptr() as u32, key.len() as u32) };
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

/// List the full host process environment (secrets included — the host
/// may disable this export via its Deny hook).
pub fn env_list() -> Result<alloc::vec::Vec<EnvEntry>, String> {
    let packed = unsafe { ffi::env_list() };
    let r: EnvListResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r.env) }
}

// ── HTTP ──

pub fn http_request(method: &str, url: &str, headers: &str, body: &[u8]) -> Result<(u32, String), String> {
    let packed = unsafe { ffi::http_request(
        method.as_ptr() as u32, method.len() as u32,
        url.as_ptr() as u32, url.len() as u32,
        headers.as_ptr() as u32, headers.len() as u32,
        if body.is_empty() { 0 } else { body.as_ptr() as u32 }, body.len() as u32,
    )};
    let r: HttpResponse = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok((r.status, r.body)) }
}

// ── Filesystem ──

pub fn fs_read(path: &str) -> Result<alloc::vec::Vec<u8>, String> {
    let packed = unsafe { ffi::fs_read(path.as_ptr() as u32, path.len() as u32) };
    let r: FsReadResult = parse_host(packed);
    if !r.error.is_empty() { return Err(r.error) }
    base64::Engine::decode(&base64::engine::general_purpose::STANDARD, &r.data)
        .map_err(|e| alloc::format!("base64 decode: {}", e))
}

pub fn fs_read_str(path: &str) -> Result<String, String> {
    let bytes = fs_read(path)?;
    String::from_utf8(bytes).map_err(|e| alloc::format!("invalid UTF-8: {}", e))
}

pub fn fs_write(path: &str, data: &[u8]) -> Result<(), String> {
    let packed = unsafe { ffi::fs_write(
        path.as_ptr() as u32, path.len() as u32,
        if data.is_empty() { 0 } else { data.as_ptr() as u32 }, data.len() as u32,
    )};
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

pub fn fs_write_str(path: &str, content: &str) -> Result<(), String> {
    fs_write(path, content.as_bytes())
}

pub fn fs_readdir(path: &str) -> Result<alloc::vec::Vec<DirEntry>, String> {
    let packed = unsafe { ffi::fs_readdir(path.as_ptr() as u32, path.len() as u32) };
    let r: FsReaddirResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r.entries) }
}

pub fn file_md5(path: &str) -> Result<String, String> {
    let packed = unsafe { ffi::file_md5(path.as_ptr() as u32, path.len() as u32) };
    let r: FileMd5Result = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r.md5) }
}

pub fn directory_md5(path: &str) -> Result<String, String> {
    let packed = unsafe { ffi::directory_md5(path.as_ptr() as u32, path.len() as u32) };
    let r: DirectoryMd5Result = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r.md5) }
}

// ── Logging ──

pub fn log_info(msg: &str) {
    unsafe { ffi::log_info(msg.as_ptr() as u32, msg.len() as u32) }
}
pub fn log_warn(msg: &str) {
    unsafe { ffi::log_warn(msg.as_ptr() as u32, msg.len() as u32) }
}
pub fn log_error(msg: &str) {
    unsafe { ffi::log_error(msg.as_ptr() as u32, msg.len() as u32) }
}

// ── Time ──

pub fn utc_now() -> u64 {
    unsafe { ffi::utc_now() }
}

// ── Runtime (agent:observers, agent:tools) ──

macro_rules! runtime_get_fn {
    ($name:ident, $ffi:ident) => {
        pub fn $name() -> Result<String, String> {
            let packed = unsafe { ffi::$ffi() };
            let r: KeyringResult = parse_host(packed);
            if !r.error.is_empty() { Err(r.error) } else { Ok(r.value) }
        }
    };
}

runtime_get_fn!(runtime_session_id, runtime_session_id);
runtime_get_fn!(runtime_user_id, runtime_user_id);
runtime_get_fn!(runtime_turn_count, runtime_turn_count);
runtime_get_fn!(runtime_model_id, runtime_model_id);
runtime_get_fn!(runtime_provider, runtime_provider);

pub fn runtime_get_metadata(key: &str) -> Result<String, String> {
    let packed = unsafe { ffi::runtime_get_metadata(key.as_ptr() as u32, key.len() as u32) };
    let r: KeyringResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(r.value) }
}


pub fn runtime_set_metadata(key: &str, val: &str) -> Result<(), String> {
    let packed = unsafe { ffi::runtime_set_metadata(key.as_ptr() as u32, key.len() as u32, val.as_ptr() as u32, val.len() as u32) };
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

pub fn runtime_set_model_config(json: &str) -> Result<(), String> {
    let packed = unsafe { ffi::runtime_set_model_config(json.as_ptr() as u32, json.len() as u32) };
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

pub fn runtime_set_embedding_config(json: &str) -> Result<(), String> {
    let packed = unsafe { ffi::runtime_set_embedding_config(json.as_ptr() as u32, json.len() as u32) };
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

pub fn runtime_set_system_prompts(json: &str) -> Result<(), String> {
    let packed = unsafe { ffi::runtime_set_system_prompts(json.as_ptr() as u32, json.len() as u32) };
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

pub fn runtime_set_max_turns(n: u32) -> Result<(), String> {
    let s = alloc::format!("{}", n);
    let packed = unsafe { ffi::runtime_set_max_turns(s.as_ptr() as u32, s.len() as u32) };
    let r: HostResult = parse_host(packed);
    if !r.error.is_empty() { Err(r.error) } else { Ok(()) }
}

// ── Internal ──

/// Convert a packed (ptr, len) u64 into bytes, using the PK convention.
fn wasm_str_packed(packed: u64) -> &'static [u8] {
    let (p, l) = up(packed);
    if p == 0 && l == 0 { return &[] }
    unsafe { core::slice::from_raw_parts(p as *const u8, l as usize) }
}

/// Parse packed host JSON into an owned value, then return the
/// host-written response buffer to the guest heap. The host allocates a
/// fresh buffer per call (via this crate's alloc export); it is dead once
/// the wrapper has deserialized it, so leaving it allocated would leak a
/// block per host call — the same leak the allocator fix eliminates
/// everywhere else.
fn parse_host<T: serde::de::DeserializeOwned + Default>(packed: u64) -> T {
    let r: T = serde_json::from_slice(wasm_str_packed(packed)).unwrap_or_default();
    sdk_dealloc(up(packed).0);
    r
}
