//! Secrets live in the system's credential store: the Keychain on a Mac, Credential Manager on Windows.

/// Reading can fail for reasons other than absence, such as the person declining the prompt.
pub trait Vault: Send + Sync {
    fn get(&self, key: &str) -> Result<Option<String>, String>;
    fn set(&self, key: &str, value: &str) -> Result<(), String>;
    fn delete(&self, key: &str) -> Result<(), String>;
}

// A development build keeps away from the installed app's entries.
const SERVICE: &str = if cfg!(debug_assertions) { "com.sharkpaw.runnerdeck.dev" } else { "com.sharkpaw.runnerdeck" };

pub struct Keychain;

fn entry(key: &str) -> Result<keyring::Entry, String> {
    keyring::Entry::new(SERVICE, key).map_err(|e| e.to_string())
}

impl Vault for Keychain {
    fn get(&self, key: &str) -> Result<Option<String>, String> {
        match entry(key)?.get_password() {
            Ok(value) => Ok(Some(value)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(e.to_string()),
        }
    }

    fn set(&self, key: &str, value: &str) -> Result<(), String> {
        entry(key)?.set_password(value).map_err(|e| e.to_string())
    }

    fn delete(&self, key: &str) -> Result<(), String> {
        match entry(key)?.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(e.to_string()),
        }
    }
}

#[cfg(test)]
pub mod testing {
    use super::Vault;
    use std::collections::HashMap;
    use std::sync::Mutex;

    #[derive(Default)]
    pub struct Memory(pub Mutex<HashMap<String, String>>);

    impl Vault for Memory {
        fn get(&self, key: &str) -> Result<Option<String>, String> {
            Ok(self.0.lock().unwrap().get(key).cloned())
        }
        fn set(&self, key: &str, value: &str) -> Result<(), String> {
            self.0.lock().unwrap().insert(key.to_string(), value.to_string());
            Ok(())
        }
        fn delete(&self, key: &str) -> Result<(), String> {
            self.0.lock().unwrap().remove(key);
            Ok(())
        }
    }

    pub struct Broken;

    impl Vault for Broken {
        fn get(&self, _: &str) -> Result<Option<String>, String> {
            Err("declined".to_string())
        }
        fn set(&self, _: &str, _: &str) -> Result<(), String> {
            Err("declined".to_string())
        }
        fn delete(&self, _: &str) -> Result<(), String> {
            Err("declined".to_string())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    #[ignore]
    fn the_real_keychain_round_trips() {
        let key = "test-round-trip";
        assert_eq!(Keychain.get(key).unwrap(), None);
        Keychain.set(key, "secret").unwrap();
        assert_eq!(Keychain.get(key).unwrap().as_deref(), Some("secret"));
        Keychain.delete(key).unwrap();
        assert_eq!(Keychain.get(key).unwrap(), None);
        Keychain.delete(key).unwrap();
    }
}
