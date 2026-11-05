use std::fs::{self, OpenOptions};
use std::net::{SocketAddr, TcpStream};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant};

// Long enough for the Manager to give its runners their own grace period.
const STOP_WAIT: Duration = Duration::from_secs(35);
const READY_WAIT: Duration = Duration::from_secs(30);

pub struct Manager {
    dir: PathBuf,
    port: u16,
    password: String,
    child: Mutex<Option<Child>>,
}

fn manager_binary() -> Result<PathBuf, String> {
    let exe = std::env::current_exe().map_err(|e| e.to_string())?;
    let dir = exe.parent().ok_or_else(|| "no app directory".to_string())?;
    Ok(dir.join(if cfg!(windows) { "runner-manager.exe" } else { "runner-manager" }))
}

/// An app opened from Finder gets a bare PATH, without Homebrew or nvm, and jobs inherit it. The
/// user's shell knows the real one.
#[cfg(unix)]
fn user_path() -> String {
    const MARK: &str = "__RUNNER_DECK_PATH__";
    let fallback = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin".to_string();
    let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/zsh".to_string());
    let script = format!("printf '{MARK}%s{MARK}' \"$PATH\"");
    let Ok(mut child) = Command::new(shell)
        .args(["-ilc", &script])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
    else {
        return fallback;
    };
    let started = Instant::now();
    loop {
        match child.try_wait() {
            Ok(Some(_)) => break,
            Ok(None) if started.elapsed() < Duration::from_secs(5) => std::thread::sleep(Duration::from_millis(50)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                return fallback;
            }
        }
    }
    let Ok(out) = child.wait_with_output() else { return fallback };
    let text = String::from_utf8_lossy(&out.stdout);
    text.split(MARK).nth(1).filter(|p| !p.is_empty()).map(str::to_string).unwrap_or(fallback)
}

#[cfg(unix)]
fn terminate(pid: u32) {
    let _ = Command::new("/bin/kill").args(["-TERM", &pid.to_string()]).status();
}

#[cfg(unix)]
fn is_our_manager(pid: u32) -> bool {
    Command::new("/bin/ps")
        .args(["-o", "comm=", "-p", &pid.to_string()])
        .output()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().ends_with("runner-manager"))
        .unwrap_or(false)
}

impl Manager {
    pub fn new(dir: PathBuf, port: u16, password: String) -> Self {
        Self { dir, port, password, child: Mutex::new(None) }
    }

    /// A crash can leave the previous Manager holding the port.
    #[cfg(unix)]
    fn stop_leftover(&self) {
        let Ok(raw) = fs::read_to_string(self.dir.join("manager.pid")) else { return };
        let Ok(pid) = raw.trim().parse::<u32>() else { return };
        if !is_our_manager(pid) {
            return;
        }
        terminate(pid);
        let started = Instant::now();
        while is_our_manager(pid) && started.elapsed() < STOP_WAIT {
            std::thread::sleep(Duration::from_millis(200));
        }
    }

    pub fn start(&self) -> Result<(), String> {
        #[cfg(unix)]
        self.stop_leftover();
        let log = OpenOptions::new()
            .create(true)
            .append(true)
            .open(self.dir.join("manager.log"))
            .map_err(|e| e.to_string())?;
        let mut cmd = Command::new(manager_binary()?);
        cmd.current_dir(&self.dir)
            .args(["-config", "config/config.yaml"])
            .env("BASIC_AUTH_USER", "admin")
            .env("BASIC_AUTH_PASSWORD", &self.password)
            .env("SERVER_ADDR", "127.0.0.1")
            .env("SERVER_PORT", self.port.to_string())
            .env("RUNNERS_BASE_PATH", self.dir.join("runners"))
            .stdin(Stdio::null())
            .stdout(log.try_clone().map_err(|e| e.to_string())?)
            .stderr(log);
        #[cfg(unix)]
        {
            cmd.env("PATH", user_path());
            // CocoaPods and others fail without a UTF-8 locale, which Finder does not set.
            if std::env::var_os("LANG").is_none() {
                cmd.env("LANG", "en_US.UTF-8");
            }
        }
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            cmd.creation_flags(0x0800_0000);
        }
        let child = cmd.spawn().map_err(|e| format!("cannot start the Manager: {e}"))?;
        fs::write(self.dir.join("manager.pid"), child.id().to_string()).map_err(|e| e.to_string())?;
        *self.child.lock().unwrap() = Some(child);
        Ok(())
    }

    /// The Manager's pid while it is alive.
    pub fn running_pid(&self) -> Option<u32> {
        let mut child = self.child.lock().unwrap();
        let child = child.as_mut()?;
        matches!(child.try_wait(), Ok(None)).then(|| child.id())
    }

    pub fn ensure_started(&self) -> Result<(), String> {
        if self.running_pid().is_some() {
            return Ok(());
        }
        self.start()
    }

    pub fn wait_ready(&self) -> Result<(), String> {
        let addr = SocketAddr::from(([127, 0, 0, 1], self.port));
        let started = Instant::now();
        while started.elapsed() < READY_WAIT {
            if TcpStream::connect_timeout(&addr, Duration::from_millis(300)).is_ok() {
                return Ok(());
            }
            if let Some(child) = self.child.lock().unwrap().as_mut() {
                if let Ok(Some(status)) = child.try_wait() {
                    return Err(format!("the Manager exited ({status}), see {}", self.dir.join("manager.log").display()));
                }
            }
            std::thread::sleep(Duration::from_millis(200));
        }
        Err(format!("the Manager did not answer, see {}", self.dir.join("manager.log").display()))
    }

    /// Asks the Manager to stop its runners and exit, and waits for it.
    pub fn stop(&self) {
        let Some(mut child) = self.child.lock().unwrap().take() else { return };
        #[cfg(unix)]
        terminate(child.id());
        #[cfg(windows)]
        let _ = child.kill();
        let started = Instant::now();
        while started.elapsed() < STOP_WAIT {
            if matches!(child.try_wait(), Ok(Some(_))) {
                let _ = fs::remove_file(self.dir.join("manager.pid"));
                return;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        let _ = child.kill();
        let _ = child.wait();
    }
}
