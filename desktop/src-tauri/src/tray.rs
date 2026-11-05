use crate::usage;
use crate::AppState;
use std::sync::Mutex;
use std::time::Duration;
use tauri::image::Image;
use tauri::menu::{CheckMenuItem, MenuBuilder, MenuItem};
use tauri::tray::{TrayIcon, TrayIconBuilder};
use tauri::{AppHandle, Manager as _, Wry};
use tauri_plugin_autostart::ManagerExt;

const SAMPLE_EVERY: Duration = Duration::from_secs(5);
const DISK_EVERY: Duration = Duration::from_secs(300);

pub struct Tray {
    icon: TrayIcon<Wry>,
    status: MenuItem<Wry>,
    usage: MenuItem<Wry>,
    toggle: MenuItem<Wry>,
    disk_kb: Mutex<Option<u64>>,
    busy_jobs: Mutex<u32>,
    tr: bool,
}

impl Tray {
    pub fn jobs(&self) -> u32 {
        *self.busy_jobs.lock().unwrap()
    }

    fn refresh(&self, running: bool, sample: Option<usage::Usage>) {
        let tr = self.tr;
        let disk = self.disk_kb.lock().unwrap().map(usage::format_size).unwrap_or_else(|| "…".to_string());
        let Some(u) = sample.filter(|_| running) else {
            *self.busy_jobs.lock().unwrap() = 0;
            let _ = self.status.set_text(if tr { "Runner'lar durduruldu" } else { "Runners stopped" });
            let _ = self.usage.set_text(format!("Disk {disk}"));
            let _ = self.toggle.set_text(if tr { "Runner'ları Başlat" } else { "Start Runners" });
            let _ = self.icon.set_title(None::<&str>);
            return;
        };
        *self.busy_jobs.lock().unwrap() = u.jobs;
        let status = if tr {
            format!("{} runner çalışıyor · {} iş sürüyor", u.runners, u.jobs)
        } else {
            format!("{} runners running · {} jobs", u.runners, u.jobs)
        };
        let cpu = u.cpu_percent.round() as u32;
        let ram = usage::format_size(u.rss_kb);
        let _ = self.status.set_text(status);
        let _ = self.usage.set_text(if tr {
            format!("CPU %{cpu} · RAM {ram} · Disk {disk}")
        } else {
            format!("CPU {cpu}% · RAM {ram} · Disk {disk}")
        });
        let _ = self.toggle.set_text(if tr { "Runner'ları Durdur" } else { "Stop Runners" });
        let short = usage::compact_size(u.rss_kb);
        let title = if tr { format!("%{cpu} {short}") } else { format!("{cpu}% {short}") };
        let _ = self.icon.set_title(Some(title));
    }
}

pub fn install(app: &AppHandle, tr: bool) -> tauri::Result<()> {
    let t = |tr_text: &'static str, en_text: &'static str| if tr { tr_text } else { en_text };
    let status = MenuItem::with_id(app, "tray_status", "…", false, None::<&str>)?;
    let usage_item = MenuItem::with_id(app, "tray_usage", "…", false, None::<&str>)?;
    let open = MenuItem::with_id(app, "tray_open", t("Paneli Aç", "Open Panel"), true, None::<&str>)?;
    let list = MenuItem::with_id(app, "tray_list", t("Bağlantılar", "Connections"), true, None::<&str>)?;
    let toggle = MenuItem::with_id(app, "tray_toggle", t("Runner'ları Durdur", "Stop Runners"), true, None::<&str>)?;
    let enabled = app.autolaunch().is_enabled().unwrap_or(false);
    let autostart =
        CheckMenuItem::with_id(app, "tray_autostart", t("Açılışta Başlat", "Open at Login"), true, enabled, None::<&str>)?;
    let quit = MenuItem::with_id(app, "tray_quit", t("Runner Deck'ten Çık", "Quit Runner Deck"), true, None::<&str>)?;
    let menu = MenuBuilder::new(app)
        .item(&status)
        .item(&usage_item)
        .separator()
        .item(&open)
        .item(&list)
        .separator()
        .item(&toggle)
        .item(&autostart)
        .separator()
        .item(&quit)
        .build()?;
    let autostart_item = autostart.clone();
    let icon = TrayIconBuilder::with_id("main")
        .icon(Image::from_bytes(include_bytes!("../icons/tray.png"))?)
        .icon_as_template(true)
        .menu(&menu)
        .show_menu_on_left_click(true)
        .on_menu_event(move |app, event| match event.id().as_ref() {
            "tray_open" => crate::open_local_in_background(app),
            "tray_list" => crate::show_connections(app),
            "tray_toggle" => crate::toggle_runners(app),
            "tray_quit" => crate::ask_to_quit(app),
            "tray_autostart" => {
                let launch = app.autolaunch();
                let want = autostart_item.is_checked().unwrap_or(false);
                // A new macOS account may not have the folder the login item goes in.
                #[cfg(target_os = "macos")]
                if let Some(home) = std::env::var_os("HOME") {
                    let _ = std::fs::create_dir_all(std::path::Path::new(&home).join("Library/LaunchAgents"));
                }
                let result = if want { launch.enable() } else { launch.disable() };
                if result.is_err() {
                    let _ = autostart_item.set_checked(!want);
                }
            }
            _ => {}
        })
        .build(app)?;
    app.manage(Tray { icon, status, usage: usage_item, toggle, disk_kb: Mutex::new(None), busy_jobs: Mutex::new(0), tr });

    let handle = app.clone();
    std::thread::spawn(move || loop {
        let dir = handle.state::<AppState>().store.lock().unwrap().dir.clone();
        *handle.state::<Tray>().disk_kb.lock().unwrap() = usage::disk_kb(&dir);
        std::thread::sleep(DISK_EVERY);
    });
    let handle = app.clone();
    std::thread::spawn(move || loop {
        let manager = &handle.state::<AppState>().manager;
        let pid = manager.running_pid();
        handle.state::<Tray>().refresh(pid.is_some(), pid.and_then(usage::sample));
        std::thread::sleep(SAMPLE_EVERY);
    });
    Ok(())
}
