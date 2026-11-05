//! Keeps the app current from GitHub Releases. An update restarts the runners, which would cancel a
//! running job, so it waits until none is running. The plugin verifies each download's signature.

use crate::{is_turkish, tray::Tray, AppState};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tauri::{AppHandle, Manager as _};
use tauri_plugin_updater::{Updater, UpdaterExt};

const FIRST_CHECK: Duration = Duration::from_secs(60);
const CHECK_EVERY: Duration = Duration::from_secs(6 * 60 * 60);
const RETRY_WHEN_BUSY: Duration = Duration::from_secs(15 * 60);

static CHECKING: AtomicBool = AtomicBool::new(false);

enum Outcome {
    UpToDate,
    WaitingForJob,
}

pub fn idle_text(app: &AppHandle, tr: bool) -> String {
    let version = &app.package_info().version;
    if tr {
        format!("Güncellemeleri Denetle (sürüm {version})")
    } else {
        format!("Check for Updates (version {version})")
    }
}

fn set_text(app: &AppHandle, text: &str) {
    if let Some(tray) = app.try_state::<Tray>() {
        tray.set_update_text(text);
    }
}

fn updater(app: &AppHandle) -> Result<Updater, String> {
    app.updater().map_err(|e| e.to_string())
}

fn jobs_running(app: &AppHandle) -> bool {
    app.try_state::<Tray>().is_some_and(|tray| tray.jobs() > 0)
}

async fn attempt(app: &AppHandle, tr: bool) -> Result<Outcome, String> {
    let Some(update) = updater(app)?.check().await.map_err(|e| e.to_string())? else {
        return Ok(Outcome::UpToDate);
    };
    if jobs_running(app) {
        return Ok(Outcome::WaitingForJob);
    }
    set_text(app, if tr { "Güncelleme indiriliyor…" } else { "Downloading update…" });
    let bytes = update.download(|_, _| {}, || {}).await.map_err(|e| e.to_string())?;
    // A job may have started during the download.
    if jobs_running(app) {
        return Ok(Outcome::WaitingForJob);
    }
    let state = app.state::<AppState>();
    state.quitting.store(true, Ordering::SeqCst);
    state.manager.stop();
    if let Err(e) = update.install(bytes) {
        state.quitting.store(false, Ordering::SeqCst);
        let _ = state.manager.ensure_started();
        return Err(e.to_string());
    }
    app.restart()
}

/// Runs one check, saying how it went in the menu, and returns when to look again.
fn check(app: &AppHandle) -> Duration {
    if CHECKING.swap(true, Ordering::SeqCst) {
        return CHECK_EVERY;
    }
    let tr = is_turkish();
    let result = tauri::async_runtime::block_on(attempt(app, tr));
    CHECKING.store(false, Ordering::SeqCst);
    match result {
        Ok(Outcome::UpToDate) => {
            set_text(app, &idle_text(app, tr));
            CHECK_EVERY
        }
        Ok(Outcome::WaitingForJob) => {
            set_text(app, if tr { "Güncelleme iş bitince kurulacak" } else { "Update waits for the running job" });
            RETRY_WHEN_BUSY
        }
        Err(e) => {
            eprintln!("update failed: {e}");
            set_text(app, if tr { "Güncelleme denenemedi, tekrar dene" } else { "Update failed, try again" });
            CHECK_EVERY
        }
    }
}

pub fn start(app: &AppHandle) {
    let app = app.clone();
    std::thread::spawn(move || {
        std::thread::sleep(FIRST_CHECK);
        loop {
            let wait = check(&app);
            std::thread::sleep(wait);
        }
    });
}

pub fn check_now(app: &AppHandle) {
    let app = app.clone();
    std::thread::spawn(move || {
        check(&app);
    });
}
