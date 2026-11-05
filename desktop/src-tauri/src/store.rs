use serde::{Deserialize, Serialize};
use std::fs;
use std::net::TcpListener;
use std::path::PathBuf;

const DEFAULT_PORT: u16 = 18484;

#[derive(Clone, Serialize, Deserialize)]
pub struct Server {
    pub id: String,
    pub name: String,
    pub url: String,
    /// Kept in desktop.json, which only this user can read.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refresh_token: Option<String>,
}

#[derive(Serialize, Deserialize)]
pub struct Store {
    #[serde(skip)]
    pub dir: PathBuf,
    pub password: String,
    pub port: u16,
    #[serde(default)]
    pub servers: Vec<Server>,
    #[serde(default = "default_last")]
    pub last: String,
}

fn default_last() -> String {
    "local".to_string()
}

fn random_hex(bytes: usize) -> Result<String, String> {
    let mut buf = vec![0u8; bytes];
    getrandom::fill(&mut buf).map_err(|e| e.to_string())?;
    Ok(buf.iter().map(|b| format!("{b:02x}")).collect())
}

fn port_free(port: u16) -> bool {
    TcpListener::bind(("127.0.0.1", port)).is_ok()
}

fn free_port() -> Result<u16, String> {
    if port_free(DEFAULT_PORT) {
        return Ok(DEFAULT_PORT);
    }
    let listener = TcpListener::bind(("127.0.0.1", 0)).map_err(|e| e.to_string())?;
    Ok(listener.local_addr().map_err(|e| e.to_string())?.port())
}

fn data_dir() -> Result<PathBuf, String> {
    let home = std::env::var_os("HOME")
        .or_else(|| std::env::var_os("USERPROFILE"))
        .ok_or_else(|| "cannot find the home directory".to_string())?;
    // Not Application Support: its space breaks build tools running inside the runners' work directories.
    Ok(PathBuf::from(home).join(".runner-deck"))
}

/// Panels are opened in a window that holds a session, so a plain http address is only acceptable
/// on this machine, where nothing crosses a network.
pub fn normalize_url(raw: &str) -> Result<String, String> {
    let url = tauri::Url::parse(raw.trim()).map_err(|_| "invalid_url".to_string())?;
    let local = matches!(url.host_str(), Some("localhost") | Some("127.0.0.1"));
    if url.scheme() != "https" && !(url.scheme() == "http" && local) {
        return Err("https_required".to_string());
    }
    if url.host_str().is_none() {
        return Err("invalid_url".to_string());
    }
    Ok(url.to_string())
}

/// Fills in and submits the local Manager's login form.
pub fn login_script(password: &str) -> String {
    let password = serde_json::to_string(password).expect("a string serializes");
    format!(
        "(function(){{var u=document.getElementById('loginUser'),p=document.getElementById('loginPass');\
         if(!u||!p)return;u.value='admin';p.value={password};p.form.submit();}})()"
    )
}

impl Store {
    pub fn load() -> Result<Self, String> {
        let dir = data_dir()?;
        fs::create_dir_all(dir.join("config")).map_err(|e| e.to_string())?;
        fs::create_dir_all(dir.join("runners")).map_err(|e| e.to_string())?;
        let path = dir.join("desktop.json");
        let mut store = match fs::read(&path) {
            Ok(raw) => serde_json::from_slice::<Store>(&raw).map_err(|e| format!("{}: {e}", path.display()))?,
            Err(_) => Store {
                dir: dir.clone(),
                password: random_hex(24)?,
                port: free_port()?,
                servers: vec![],
                last: default_last(),
            },
        };
        store.dir = dir;
        store.write_config()?;
        store.save()?;
        Ok(store)
    }

    fn write_config(&self) -> Result<(), String> {
        let path = self.dir.join("config").join("config.yaml");
        if path.exists() {
            return Ok(());
        }
        let base = self.dir.join("runners");
        let body = format!("runners:\n  base_path: {}\n  items: []\n", serde_json::to_string(&base).map_err(|e| e.to_string())?);
        fs::write(path, body).map_err(|e| e.to_string())
    }

    pub fn save(&self) -> Result<(), String> {
        let path = self.dir.join("desktop.json");
        let body = serde_json::to_vec_pretty(self).map_err(|e| e.to_string())?;
        fs::write(&path, body).map_err(|e| e.to_string())?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).map_err(|e| e.to_string())?;
        }
        Ok(())
    }

    pub fn add_server(&mut self, name: &str, url: &str) -> Result<Server, String> {
        let url = normalize_url(url)?;
        let name = name.trim();
        let server = Server {
            id: random_hex(8)?,
            name: if name.is_empty() { url.clone() } else { name.to_string() },
            url,
            refresh_token: None,
        };
        self.servers.push(server.clone());
        self.save()?;
        Ok(server)
    }

    pub fn set_refresh_token(&mut self, id: &str, token: Option<String>) -> Result<(), String> {
        if let Some(server) = self.servers.iter_mut().find(|s| s.id == id) {
            server.refresh_token = token;
        }
        self.save()
    }

    pub fn remove_server(&mut self, id: &str) -> Result<(), String> {
        self.servers.retain(|s| s.id != id);
        if self.last == id {
            self.last = default_last();
        }
        self.save()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_https_leaves_this_machine() {
        assert!(normalize_url("https://deck.example.com").is_ok());
        assert!(normalize_url("http://127.0.0.1:8080").is_ok());
        assert_eq!(normalize_url("http://deck.example.com").unwrap_err(), "https_required");
        assert_eq!(normalize_url("file:///etc/passwd").unwrap_err(), "https_required");
        assert_eq!(normalize_url("deck").unwrap_err(), "invalid_url");
    }

    #[test]
    fn login_script_escapes_the_password() {
        assert!(login_script("a\"b").contains(r#"p.value="a\"b""#));
    }
}
