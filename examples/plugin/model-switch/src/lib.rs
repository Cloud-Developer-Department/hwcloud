// model-switch plugin — agent:observers example demonstrating runtime_* host API.
//
// On every model.call.leave:
//   - Reads session_id, user_id, model_id, turn_count via runtime_get_*
//   - Writes turn metadata via runtime_set_metadata
//   - On API errors (401/403/429): rotates to backup model via runtime_set_model_config

#![no_std]
#![no_main]

extern crate alloc;
use hwcloud_pdk::prelude::*;
use hwcloud_pdk::export::Plugin;

struct ModelSwitch;
impl Plugin for ModelSwitch {
    fn plugin_type() -> &'static str { "agent:observers" }
    fn name() -> &'static str { "model-switch" }
    fn stage_filter() -> (&'static str, &'static str) { ("model.call", "leave") }

    fn observe_stage(event: &StageInput) -> StageOutput {
        // ── Read runtime state on every model.call.leave ──
        let sid = host::runtime_session_id().unwrap_or_default();
        let uid = host::runtime_user_id().unwrap_or_default();
        let mid = host::runtime_model_id().unwrap_or_default();
        let turn = host::runtime_turn_count().unwrap_or_default();

        host::log_info(&alloc::format!(
            "model.call.leave: session={} user={} model={} turn={}",
            sid, uid, mid, turn
        ));

        // Persist the last model used as session metadata.
        let _ = host::runtime_set_metadata("last_model", &mid);
        let _ = host::runtime_set_metadata("last_turn", &turn);

        // ── On API error: rotate model ──
        if !event.error.is_empty() {
            let err = &event.error;
            host::log_warn(&alloc::format!("model.call error: {}", err));

            let backup = host::keyring_get("hwcloud", "backup_model_config");
            if let Ok(cfg_json) = backup {
                if !cfg_json.is_empty() {
                    host::log_info("model-switch: rotating to backup model");
                    if let Err(e) = host::runtime_set_model_config(&cfg_json) {
                        host::log_error(&alloc::format!("set_model_config failed: {}", e));
                    }
                }
            }
        }

        StageOutput { action: String::from("continue"), reason: String::new() }
    }
}

hwcloud_pdk::export!(ModelSwitch);
