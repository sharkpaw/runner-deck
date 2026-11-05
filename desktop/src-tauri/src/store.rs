use serde::{Deserialize, Serialize};
use std::fs;
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use crate::vault::Vault;

const DEFAULT_PORT: u16 = 18484;
const PASSWORD_KEY: &str = "local-password";

fn token_key(server_id: &str) -> String {
    format!("server-{server_id}")
}

#[derive(Clone, Serialize, Deserialize)]
pub struct Server {
    pub id: String,
    pub name: String,
    pub url: String,
    /// Whether the credential store holds a session, so listing servers needs no Keychain access.
    #[serde(default)]
    pub signed_in: bool,
    /// Only where the credential store refused it, or in a file from before it was used.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    refresh_token: Option<String>,
}

#[derive(Serialize, Deserialize)]
pub struct Store {
    #[serde(skip)]
    pub dir: PathBuf,
    #[serde(skip)]
    pub password: String,
    /// Only where the credential store refused it, or in a file from before it was used.
    #[serde(default, rename = "password", skip_serializing_if = "String::is_empty")]
    file_password: String,
    #[serde(skip)]
    vault: Option<Arc<dyn Vault>>,
    #[serde(default = "free_port_or_default")]
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

fn free_port_or_default() -> u16 {
    free_port().unwrap_or(DEFAULT_PORT)
}

fn free_port() -> Result<u16, String> {
    if port_free(DEFAULT_PORT) {
        return Ok(DEFAULT_PORT);
    }
    let listener = TcpListener::bind(("127.0.0.1", 0)).map_err(|e| e.to_string())?;
    Ok(listener.local_addr().map_err(|e| e.to_string())?.port())
}

fn data_dir() -> Result<PathBuf, String> {
    // A development build can run beside an installed app, whose data it must not touch.
    if cfg!(debug_assertions) {
        if let Some(dir) = std::env::var_os("RUNNER_DECK_DEV_DIR") {
            return Ok(PathBuf::from(dir));
        }
    }
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
    pub fn load(vault: Arc<dyn Vault>) -> Result<Self, String> {
        Self::load_from(data_dir()?, vault)
    }

    fn load_from(dir: PathBuf, vault: Arc<dyn Vault>) -> Result<Self, String> {
        fs::create_dir_all(dir.join("config")).map_err(|e| e.to_string())?;
        fs::create_dir_all(dir.join("runners")).map_err(|e| e.to_string())?;
        let mut store = Self::read_file(&dir)?;
        store.dir = dir;
        store.vault = Some(vault);
        store.settle_password()?;
        store.settle_tokens();
        store.write_config()?;
        store.save()?;
        Ok(store)
    }

    fn read_file(dir: &Path) -> Result<Self, String> {
        let path = dir.join("desktop.json");
        match fs::read(&path) {
            Ok(raw) => serde_json::from_slice(&raw).map_err(|e| format!("{}: {e}", path.display())),
            Err(_) => Ok(Store {
                dir: dir.to_path_buf(),
                password: String::new(),
                file_password: String::new(),
                vault: None,
                port: free_port()?,
                servers: vec![],
                last: default_last(),
            }),
        }
    }

    fn vault(&self) -> &dyn Vault {
        self.vault.as_deref().expect("set by load")
    }

    /// Moves a password out of the file. When the credential store cannot be used the app still has
    /// to run: a new password stays in the file, which only this user can read, and one that exists
    /// but was declined is replaced for this run only, which signs the local panel in again.
    fn settle_password(&mut self) -> Result<(), String> {
        if !self.file_password.is_empty() {
            self.password = self.file_password.clone();
            if self.vault().set(PASSWORD_KEY, &self.password).is_ok() {
                self.file_password.clear();
            }
            return Ok(());
        }
        match self.vault().get(PASSWORD_KEY) {
            Ok(Some(password)) => self.password = password,
            Ok(None) => {
                self.password = random_hex(24)?;
                if self.vault().set(PASSWORD_KEY, &self.password).is_err() {
                    self.file_password = self.password.clone();
                }
            }
            Err(e) => {
                eprintln!("cannot read the local password from the credential store: {e}");
                self.password = random_hex(24)?;
            }
        }
        Ok(())
    }

    fn settle_tokens(&mut self) {
        let vault = self.vault.clone().expect("set by load");
        for server in &mut self.servers {
            if let Some(token) = server.refresh_token.clone() {
                server.signed_in = true;
                if vault.set(&token_key(&server.id), &token).is_ok() {
                    server.refresh_token = None;
                }
            }
        }
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
            signed_in: false,
            refresh_token: None,
        };
        self.servers.push(server.clone());
        self.save()?;
        Ok(server)
    }

    /// The saved session of a server. Declining the Keychain prompt reads as having none.
    pub fn refresh_token(&self, id: &str) -> Option<String> {
        let server = self.servers.iter().find(|s| s.id == id)?;
        if server.refresh_token.is_some() {
            return server.refresh_token.clone();
        }
        if !server.signed_in {
            return None;
        }
        self.vault().get(&token_key(id)).ok().flatten()
    }

    pub fn set_refresh_token(&mut self, id: &str, token: Option<String>) -> Result<(), String> {
        let vault = self.vault.clone().expect("set by load");
        let Some(server) = self.servers.iter_mut().find(|s| s.id == id) else { return Ok(()) };
        server.signed_in = token.is_some();
        server.refresh_token = match &token {
            Some(value) => vault.set(&token_key(id), value).err().map(|_| value.clone()),
            None => {
                let _ = vault.delete(&token_key(id));
                None
            }
        };
        self.save()
    }

    pub fn remove_server(&mut self, id: &str) -> Result<(), String> {
        let _ = self.vault().delete(&token_key(id));
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

    use crate::vault::testing::{Broken, Memory};

    fn temp_dir(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("rd-store-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn file_text(dir: &Path) -> String {
        fs::read_to_string(dir.join("desktop.json")).unwrap()
    }

    #[test]
    fn a_new_install_keeps_secrets_out_of_the_file() {
        let dir = temp_dir("new");
        let vault = Arc::new(Memory::default());
        let mut store = Store::load_from(dir.clone(), vault.clone()).unwrap();
        assert!(!store.password.is_empty());
        assert_eq!(vault.get(PASSWORD_KEY).unwrap().as_deref(), Some(store.password.as_str()));
        let id = store.add_server("s", "https://deck.example.com").unwrap().id;
        store.set_refresh_token(&id, Some("refresh-secret".to_string())).unwrap();

        let text = file_text(&dir);
        assert!(!text.contains(&store.password) && !text.contains("refresh-secret"), "{text}");
        assert!(text.contains("\"signed_in\": true"), "{text}");
        assert_eq!(store.refresh_token(&id).as_deref(), Some("refresh-secret"));

        let again = Store::load_from(dir.clone(), vault.clone()).unwrap();
        assert_eq!(again.password, store.password);
        assert_eq!(again.refresh_token(&id).as_deref(), Some("refresh-secret"));

        store.set_refresh_token(&id, None).unwrap();
        assert_eq!(store.refresh_token(&id), None);
        store.set_refresh_token(&id, Some("again".to_string())).unwrap();
        store.remove_server(&id).unwrap();
        assert_eq!(vault.get(&token_key(&id)).unwrap(), None);
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn an_old_file_moves_into_the_vault() {
        let dir = temp_dir("old");
        fs::write(
            dir.join("desktop.json"),
            r#"{"password":"old-password","port":18555,"last":"a","servers":[{"id":"a","name":"n","url":"https://x/","refresh_token":"old-token"}]}"#,
        )
        .unwrap();
        let vault = Arc::new(Memory::default());
        let store = Store::load_from(dir.clone(), vault.clone()).unwrap();
        assert_eq!(store.password, "old-password");
        assert_eq!(store.port, 18555);
        assert_eq!(vault.get(PASSWORD_KEY).unwrap().as_deref(), Some("old-password"));
        assert_eq!(store.refresh_token("a").as_deref(), Some("old-token"));
        let text = file_text(&dir);
        assert!(!text.contains("old-password") && !text.contains("old-token"), "{text}");
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_refusing_vault_loses_nothing() {
        let dir = temp_dir("refuse");
        fs::write(
            dir.join("desktop.json"),
            r#"{"password":"old-password","port":18555,"servers":[{"id":"a","name":"n","url":"https://x/","refresh_token":"old-token"}]}"#,
        )
        .unwrap();
        let mut store = Store::load_from(dir.clone(), Arc::new(Broken)).unwrap();
        assert_eq!(store.password, "old-password");
        assert_eq!(store.refresh_token("a").as_deref(), Some("old-token"));
        store.set_refresh_token("a", Some("newer".to_string())).unwrap();
        assert_eq!(store.refresh_token("a").as_deref(), Some("newer"));
        let text = file_text(&dir);
        assert!(text.contains("old-password") && text.contains("newer"), "{text}");
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_declined_password_is_replaced_for_this_run_only() {
        let dir = temp_dir("declined");
        let store = Store::load_from(dir.clone(), Arc::new(Broken)).unwrap();
        assert!(!store.password.is_empty());
        assert!(!file_text(&dir).contains(&store.password));
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn login_script_escapes_the_password() {
        assert!(login_script("a\"b").contains(r#"p.value="a\"b""#));
    }
}
