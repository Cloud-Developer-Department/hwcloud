//! envsync — example scheduled-job plugin (agent:tools).
//!
//! Declares one cron job that runs every 5 minutes: reads the secret
//! "PLUGIN_SECRET" from the keyring and syncs it into the host process
//! environment as HWCLOUD_PLUGIN_SECRET. Demonstrates the three pieces
//! working together:
//!
//!   - metadata() carries the schedules declaration
//!   - the host registers the cron job at load time and calls
//!     run_scheduled when it fires
//!   - the job body uses host::keyring_get + host::env_set

#![no_std]
#![no_main]

extern crate alloc;
use hwcloud_pdk::prelude::*;
use hwcloud_pdk::export::Plugin;

pub struct EnvSyncPlugin;

impl Plugin for EnvSyncPlugin {
    fn name() -> &'static str { "envsync" }
    fn description() -> &'static str {
        "every 5 min: sync keyring secret PLUGIN_SECRET into env HWCLOUD_PLUGIN_SECRET"
    }

    fn scheduled_jobs() -> Vec<ScheduledJob> {
        vec![ScheduledJob {
            id: "sync-keyring-env".into(),
            cron: "*/5 * * * *".into(),
            description: "sync keyring secret into the host environment".into(),
        }]
    }

    fn run_scheduled_job(job: &ScheduledJobInput) -> Result<String, String> {
        // keyring_get on a missing key returns "" without error (the host
        // keyring API maps not-found to an empty value) — the plugin owns
        // the business rule: a missing secret is a failure, not something
        // to write into the environment.
        let secret = host::keyring_get("hwcloud", "PLUGIN_SECRET")?;
        if secret.is_empty() {
            return Err("PLUGIN_SECRET not found in keyring".into());
        }
        host::env_set("HWCLOUD_PLUGIN_SECRET", &secret)?;
        Ok(format!(
            "job {} at {}: synced {} bytes",
            job.id, job.scheduled_at, secret.len()
        ))
    }
}

hwcloud_pdk::export!(EnvSyncPlugin);
