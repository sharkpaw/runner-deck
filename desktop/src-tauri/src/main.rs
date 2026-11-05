#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod manager;
mod oauth;
mod store;
mod tray;
mod updater;
mod usage;
mod vault;

use manager::Manager;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use store::{Server, Store};
use tauri::menu::{Menu, MenuBuilder, MenuItem, PredefinedMenuItem, SubmenuBuilder};
use tauri::webview::PageLoadEvent;
use tauri::{AppHandle, Manager as _, RunEvent, Url, WebviewUrl, WebviewWindow, WebviewWindowBuilder, Wry};

pub struct AppState {
    pub store: Mutex<Store>,
    pub manager: Manager,
    // One login attempt per opening, so a wrong password cannot loop.
    login_tried: AtomicBool,
    // One attempt per opening, for the same reason.
    resume_tried: AtomicBool,
    booted: AtomicBool,
    pub quitting: AtomicBool,
}

// What the connections page sees of a server, which never includes its token.
#[derive(serde::Serialize)]
struct ServerView {
    id: String,
    name: String,
    url: String,
    signed_in: bool,
}

#[derive(serde::Serialize)]
struct Connections {
    servers: Vec<ServerView>,
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
        servers: store
            .servers
            .iter()
            .map(|s| ServerView { id: s.id.clone(), name: s.name.clone(), url: s.url.clone(), signed_in: s.signed_in })
            .collect(),
        last: store.last.clone(),
        first_load: !state.booted.swap(true, Ordering::SeqCst),
    }
}

// Blocks until the Manager answers, so it runs off the main thread.
#[tauri::command(async)]
fn open_local(app: AppHandle, state: tauri::State<AppState>) -> Result<(), String> {
    state.manager.ensure_started()?;
    state.manager.wait_ready()?;
    let port = state.store.lock().unwrap().port;
    state.login_tried.store(false, Ordering::SeqCst);
    remember(&state, "local");
    let url = Url::parse(&format!("http://127.0.0.1:{port}/")).map_err(|e| e.to_string())?;
    main_window(&app)?.navigate(url).map_err(|e| e.to_string())
}

// Can wait minutes for the person to sign in, so it runs off the main thread.
#[tauri::command(async)]
fn open_server(app: AppHandle, state: tauri::State<AppState>, id: String) -> Result<(), String> {
    let (url, signed_in) = {
        let store = state.store.lock().unwrap();
        let server = store.servers.iter().find(|s| s.id == id).ok_or_else(|| "unknown server".to_string())?;
        (server.url.clone(), server.signed_in)
    };
    remember(&state, &id);
    // The window's own cookie lasts as long as the session, so usually no token is needed at all, and
    // neither is the Keychain. If the cookie has gone, the page load below signs it in again.
    if signed_in {
        state.resume_tried.store(false, Ordering::SeqCst);
        return main_window(&app)?.navigate(Url::parse(&url).map_err(|e| e.to_string())?).map_err(|e| e.to_string());
    }
    let done = if is_turkish() {
        "Giriş tamamlandı, Runner Deck'e dönebilirsin."
    } else {
        "Signed in. You can go back to Runner Deck."
    };
    let opened = oauth::open(&url, None, &open_external, done)?;
    state.store.lock().unwrap().set_refresh_token(&id, opened.refresh_token)?;
    show_main(&app);
    main_window(&app)?.navigate(Url::parse(&opened.url).map_err(|e| e.to_string())?).map_err(|e| e.to_string())
}

// A signed-in server showing its login page has lost the window's cookie. Logging out also lands
// here, and then the server refuses the token, so the login page stays and no browser opens.
fn resume_session(app: &AppHandle, page: &Url) {
    let state = app.state::<AppState>();
    let found = {
        let store = state.store.lock().unwrap();
        store.servers.iter().find(|s| s.signed_in && same_origin(&s.url, page)).map(|s| (s.id.clone(), s.url.clone()))
    };
    let Some((id, url)) = found else { return };
    if state.resume_tried.swap(true, Ordering::SeqCst) {
        return;
    }
    let app = app.clone();
    std::thread::spawn(move || {
        let state = app.state::<AppState>();
        let token = state.store.lock().unwrap().refresh_token(&id);
        let Some(token) = token else { return };
        match oauth::resume(&url, &token) {
            Ok(Some(opened)) => {
                let _ = state.store.lock().unwrap().set_refresh_token(&id, opened.refresh_token);
                if let (Ok(window), Ok(target)) = (main_window(&app), Url::parse(&opened.url)) {
                    let _ = window.navigate(target);
                }
            }
            Ok(None) => {
                let _ = state.store.lock().unwrap().set_refresh_token(&id, None);
            }
            Err(e) => eprintln!("cannot resume the session: {e}"),
        }
    });
}

fn same_origin(server: &str, page: &Url) -> bool {
    Url::parse(server).is_ok_and(|s| s.origin() == page.origin())
}

#[tauri::command]
fn add_server(state: tauri::State<AppState>, name: String, url: String) -> Result<String, String> {
    state.store.lock().unwrap().add_server(&name, &url).map(|server: Server| server.id)
}

#[tauri::command]
fn remove_server(state: tauri::State<AppState>, id: String) -> Result<(), String> {
    let mut store = state.store.lock().unwrap();
    // Forgetting a server also signs out of it, so no live session is left behind.
    let url = store.servers.iter().find(|s| s.id == id).map(|s| s.url.clone());
    if let (Some(url), Some(token)) = (url, store.refresh_token(&id)) {
        std::thread::spawn(move || oauth::revoke(&url, &token));
    }
    store.remove_server(&id)
}

fn remember(state: &AppState, id: &str) {
    let mut store = state.store.lock().unwrap();
    store.last = id.to_string();
    let _ = store.save();
}

pub fn show_main(app: &AppHandle) {
    #[cfg(target_os = "macos")]
    let _ = app.set_activation_policy(tauri::ActivationPolicy::Regular);
    if let Ok(window) = main_window(app) {
        let _ = window.show();
        let _ = window.set_focus();
    }
}

// With the window closed the app lives in the menu bar only, with no Dock icon.
fn hide_main(app: &AppHandle) {
    if let Ok(window) = main_window(app) {
        let _ = window.hide();
    }
    #[cfg(target_os = "macos")]
    let _ = app.set_activation_policy(tauri::ActivationPolicy::Accessory);
}

pub fn show_connections(app: &AppHandle) {
    show_main(app);
    if let Ok(window) = main_window(app) {
        let _ = window.navigate(connections_url());
    }
}

pub fn open_local_in_background(app: &AppHandle) {
    show_main(app);
    let app = app.clone();
    std::thread::spawn(move || {
        let state = app.state::<AppState>();
        let opened = state.manager.ensure_started().and_then(|_| state.manager.wait_ready()).and_then(|_| {
            let port = state.store.lock().unwrap().port;
            state.login_tried.store(false, Ordering::SeqCst);
            remember(&state, "local");
            let url = Url::parse(&format!("http://127.0.0.1:{port}/")).map_err(|e| e.to_string())?;
            main_window(&app)?.navigate(url).map_err(|e| e.to_string())
        });
        if opened.is_err() {
            show_connections(&app);
        }
    });
}

fn stop_runners(app: &AppHandle) {
    // The panel it was showing is about to go away.
    let on_local = main_window(app)
        .ok()
        .and_then(|w| w.url().ok())
        .is_some_and(|url| url.host_str() == Some("127.0.0.1"));
    if on_local {
        if let Ok(window) = main_window(app) {
            let _ = window.navigate(connections_url());
        }
    }
    let app = app.clone();
    std::thread::spawn(move || app.state::<AppState>().manager.stop());
}

// Stopping cancels running jobs, so only then is it worth a question.
pub fn toggle_runners(app: &AppHandle) {
    if app.state::<AppState>().manager.running_pid().is_none() {
        let app = app.clone();
        std::thread::spawn(move || {
            let _ = app.state::<AppState>().manager.ensure_started();
        });
    } else if app.state::<tray::Tray>().jobs() > 0 {
        open_question(app, "quit.html#stop");
    } else {
        stop_runners(app);
    }
}

pub fn is_turkish() -> bool {
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

fn open_question(app: &AppHandle, page: &'static str) {
    if let Some(window) = app.get_webview_window("quit") {
        let _ = window.close();
    }
    // Creating a window from an event handler can deadlock on Windows.
    let app = app.clone();
    std::thread::spawn(move || {
        if let Ok(window) = WebviewWindowBuilder::new(&app, "quit", WebviewUrl::App(page.into()))
            .title("Runner Deck")
            .inner_size(440.0, 190.0)
            .resizable(false)
            .minimizable(false)
            .maximizable(false)
            .always_on_top(true)
            .center()
            .build()
        {
            let _ = window.set_focus();
        }
    });
}

// Quitting stops the runners, and with them any running job, so it is worth one question.
pub fn ask_to_quit(app: &AppHandle) {
    if !app.state::<AppState>().quitting.load(Ordering::SeqCst) {
        open_question(app, "quit.html");
    }
}

#[tauri::command]
fn stop_confirmed(app: AppHandle) {
    if let Some(window) = app.get_webview_window("quit") {
        let _ = window.close();
    }
    stop_runners(&app);
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

// Passed when started at login, so the app comes up in the menu bar with no window.
const HIDDEN_ARG: &str = "--hidden";

fn main() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            Some(vec![HIDDEN_ARG]),
        ))
        .plugin(tauri_plugin_updater::Builder::new().build())
        .invoke_handler(tauri::generate_handler![connections, open_local, open_server, add_server, remove_server, quit_confirmed, stop_confirmed])
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
            } else if url.path() == "/login" {
                resume_session(webview.app_handle(), url);
            }
        })
        .setup(|app| {
            let store = Store::load(std::sync::Arc::new(vault::Keychain))?;
            let manager = Manager::new(store.dir.clone(), store.port, store.password.clone());
            manager.start()?;
            app.manage(AppState {
                store: Mutex::new(store),
                manager,
                login_tried: AtomicBool::new(false),
                resume_tried: AtomicBool::new(false),
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
                    "local" => open_local_in_background(app),
                    _ => {}
                }
            });
            tray::install(&handle, is_turkish())?;
            updater::start(&handle);
            let hidden = std::env::args().any(|arg| arg == HIDDEN_ARG);
            if hidden {
                #[cfg(target_os = "macos")]
                let _ = handle.set_activation_policy(tauri::ActivationPolicy::Accessory);
            }
            WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .visible(!hidden)
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
                hide_main(window.app_handle());
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
            show_main(app);
        }
        _ => {}
    });
}
