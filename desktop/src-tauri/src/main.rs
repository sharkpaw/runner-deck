#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod manager;
mod store;

use manager::Manager;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use store::{Server, Store};
use tauri::menu::{Menu, MenuBuilder, MenuItem, PredefinedMenuItem, SubmenuBuilder};
use tauri::webview::PageLoadEvent;
use tauri::{AppHandle, Manager as _, RunEvent, Url, WebviewUrl, WebviewWindow, WebviewWindowBuilder, Wry};

struct AppState {
    store: Mutex<Store>,
    manager: Manager,
    // One login attempt per opening, so a wrong password cannot loop.
    login_tried: AtomicBool,
    booted: AtomicBool,
    quitting: AtomicBool,
}

#[derive(serde::Serialize)]
struct Connections {
    servers: Vec<Server>,
    last: String,
    first_load: bool,
}

fn main_window(app: &AppHandle) -> Result<WebviewWindow, String> {
    app.get_webview_window("main").ok_or_else(|| "no window".to_string())
}

fn connections_url() -> Url {
    let base = if cfg!(windows) { "http://tauri.localhost" } else { "tauri://localhost" };
    Url::parse(&format!("{base}/index.html#list")).expect("valid url")
}

#[tauri::command]
fn connections(state: tauri::State<AppState>) -> Connections {
    let store = state.store.lock().unwrap();
    Connections {
        servers: store.servers.clone(),
        last: store.last.clone(),
        first_load: !state.booted.swap(true, Ordering::SeqCst),
    }
}

// Blocks until the Manager answers, so it runs off the main thread.
#[tauri::command(async)]
fn open_local(app: AppHandle, state: tauri::State<AppState>) -> Result<(), String> {
    state.manager.wait_ready()?;
    let port = state.store.lock().unwrap().port;
    state.login_tried.store(false, Ordering::SeqCst);
    remember(&state, "local");
    let url = Url::parse(&format!("http://127.0.0.1:{port}/")).map_err(|e| e.to_string())?;
    main_window(&app)?.navigate(url).map_err(|e| e.to_string())
}

#[tauri::command]
fn open_server(app: AppHandle, state: tauri::State<AppState>, id: String) -> Result<(), String> {
    let url = state.store.lock().unwrap().servers.iter().find(|s| s.id == id).map(|s| s.url.clone());
    let url = url.ok_or_else(|| "unknown server".to_string())?;
    remember(&state, &id);
    main_window(&app)?.navigate(Url::parse(&url).map_err(|e| e.to_string())?).map_err(|e| e.to_string())
}

#[tauri::command]
fn add_server(state: tauri::State<AppState>, name: String, url: String) -> Result<Server, String> {
    state.store.lock().unwrap().add_server(&name, &url)
}

#[tauri::command]
fn remove_server(state: tauri::State<AppState>, id: String) -> Result<(), String> {
    state.store.lock().unwrap().remove_server(&id)
}

fn remember(state: &AppState, id: &str) {
    let mut store = state.store.lock().unwrap();
    store.last = id.to_string();
    let _ = store.save();
}

fn is_turkish() -> bool {
    sys_locale::get_locale().is_some_and(|l| l.to_lowercase().starts_with("tr"))
}

fn build_menu(app: &AppHandle) -> tauri::Result<Menu<Wry>> {
    let tr = is_turkish();
    let t = |tr_text: &'static str, en_text: &'static str| if tr { tr_text } else { en_text };
    let quit = MenuItem::with_id(app, "quit", t("Runner Deck'ten Çık", "Quit Runner Deck"), true, Some("CmdOrCtrl+Q"))?;
    let app_menu = SubmenuBuilder::new(app, "Runner Deck")
        .item(&PredefinedMenuItem::about(app, None, None)?)
        .separator()
        .item(&PredefinedMenuItem::hide(app, None)?)
        .separator()
        .item(&quit)
        .build()?;
    let edit = SubmenuBuilder::new(app, t("Düzen", "Edit"))
        .undo()
        .redo()
        .separator()
        .cut()
        .copy()
        .paste()
        .select_all()
        .build()?;
    let local = MenuItem::with_id(app, "local", t("Bu Bilgisayar", "This Computer"), true, Some("CmdOrCtrl+1"))?;
    let list = MenuItem::with_id(app, "list", t("Bağlantılar", "Connections"), true, Some("CmdOrCtrl+K"))?;
    let reload = MenuItem::with_id(app, "reload", t("Yenile", "Reload"), true, Some("CmdOrCtrl+R"))?;
    let view = SubmenuBuilder::new(app, t("Bağlantı", "Connection"))
        .item(&local)
        .item(&list)
        .separator()
        .item(&reload)
        .build()?;
    let window = SubmenuBuilder::new(app, t("Pencere", "Window"))
        .item(&PredefinedMenuItem::minimize(app, None)?)
        .item(&PredefinedMenuItem::maximize(app, None)?)
        .build()?;
    MenuBuilder::new(app).item(&app_menu).item(&edit).item(&view).item(&window).build()
}

// Quitting stops the runners, and with them any running job, so it is worth one question.
fn ask_to_quit(app: &AppHandle) {
    if app.state::<AppState>().quitting.load(Ordering::SeqCst) {
        return;
    }
    if let Some(window) = app.get_webview_window("quit") {
        let _ = window.set_focus();
        return;
    }
    // Creating a window from an event handler can deadlock on Windows.
    let app = app.clone();
    std::thread::spawn(move || {
        let _ = WebviewWindowBuilder::new(&app, "quit", WebviewUrl::App("quit.html".into()))
            .title("Runner Deck")
            .inner_size(440.0, 190.0)
            .resizable(false)
            .minimizable(false)
            .maximizable(false)
            .always_on_top(true)
            .center()
            .build();
    });
}

#[tauri::command]
fn quit_confirmed(app: AppHandle, state: tauri::State<AppState>) {
    state.quitting.store(true, Ordering::SeqCst);
    for window in app.webview_windows().values() {
        let _ = window.hide();
    }
    std::thread::spawn(move || {
        app.state::<AppState>().manager.stop();
        app.exit(0);
    });
}

fn open_external(url: &Url) {
    if url.scheme() != "https" && url.scheme() != "http" {
        return;
    }
    #[cfg(target_os = "macos")]
    let _ = std::process::Command::new("/usr/bin/open").arg(url.as_str()).spawn();
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        let _ = std::process::Command::new("rundll32")
            .args(["url.dll,FileProtocolHandler", url.as_str()])
            .creation_flags(0x0800_0000)
            .spawn();
    }
}

fn main() {
    let app = tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![connections, open_local, open_server, add_server, remove_server, quit_confirmed])
        .on_page_load(|webview, payload| {
            if payload.event() != PageLoadEvent::Finished {
                return;
            }
            let state = webview.state::<AppState>();
            let (port, password) = {
                let store = state.store.lock().unwrap();
                (store.port, store.password.clone())
            };
            let url = payload.url();
            // Only ever sent to our own Manager's login page.
            let ours = url.host_str() == Some("127.0.0.1") && url.port() == Some(port) && url.path() == "/login";
            if ours && !state.login_tried.swap(true, Ordering::SeqCst) {
                let _ = webview.eval(store::login_script(&password));
            } else if url.host_str() == Some("127.0.0.1") && url.path() != "/login" {
                state.login_tried.store(false, Ordering::SeqCst);
            }
        })
        .setup(|app| {
            let store = Store::load()?;
            let manager = Manager::new(store.dir.clone(), store.port, store.password.clone());
            manager.start()?;
            app.manage(AppState {
                store: Mutex::new(store),
                manager,
                login_tried: AtomicBool::new(false),
                booted: AtomicBool::new(false),
                quitting: AtomicBool::new(false),
            });
            let handle = app.handle().clone();
            let menu = build_menu(&handle)?;
            app.set_menu(menu)?;
            app.on_menu_event(|app, event| {
                let Ok(window) = main_window(app) else { return };
                match event.id().as_ref() {
                    "quit" => ask_to_quit(app),
                    "list" => {
                        let _ = window.navigate(connections_url());
                    }
                    "reload" => {
                        let _ = window.eval("location.reload()");
                    }
                    "local" => {
                        let port = app.state::<AppState>().store.lock().unwrap().port;
                        if let Ok(url) = Url::parse(&format!("http://127.0.0.1:{port}/")) {
                            let state = app.state::<AppState>();
                            state.login_tried.store(false, Ordering::SeqCst);
                            remember(&state, "local");
                            let _ = window.navigate(url);
                        }
                    }
                    _ => {}
                }
            });
            WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title("Runner Deck")
                .inner_size(1280.0, 860.0)
                .min_inner_size(900.0, 600.0)
                .on_new_window(|url, _features| {
                    open_external(&url);
                    tauri::webview::NewWindowResponse::Deny
                })
                .build()?;
            Ok(())
        })
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                if window.label() != "main" {
                    return;
                }
                api.prevent_close();
                if cfg!(target_os = "macos") {
                    let _ = window.hide();
                } else {
                    ask_to_quit(window.app_handle());
                }
            }
        })
        .build(tauri::generate_context!())
        .expect("error while building the app");

    app.run(|app, event| match event {
        RunEvent::ExitRequested { api, .. } => {
            if !app.state::<AppState>().quitting.load(Ordering::SeqCst) {
                api.prevent_exit();
                ask_to_quit(app);
            }
        }
        RunEvent::Exit => app.state::<AppState>().manager.stop(),
        #[cfg(target_os = "macos")]
        RunEvent::Reopen { .. } => {
            if let Ok(window) = main_window(app) {
                let _ = window.show();
                let _ = window.set_focus();
            }
        }
        _ => {}
    });
}
