//! Signs the app in to a Runner Deck server: OAuth 2.1 with PKCE in the system browser, where the
//! user's password manager works, coming back to a one-shot listener on this machine.

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::io::{BufRead, BufReader, ErrorKind, Write};
use std::net::{TcpListener, TcpStream};
use std::time::{Duration, Instant};
use tauri::Url;

const CLIENT_ID: &str = "runner-deck-desktop";
const LOGIN_WAIT: Duration = Duration::from_secs(300);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);

/// What opening a server produced: where to point the window, and the refresh token to keep.
pub struct Opened {
    pub url: String,
    pub refresh_token: Option<String>,
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    refresh_token: String,
}

#[derive(Deserialize)]
struct TicketResponse {
    url: String,
}

fn agent() -> ureq::Agent {
    ureq::Agent::config_builder()
        .http_status_as_error(false)
        .max_redirects(0)
        .timeout_global(Some(REQUEST_TIMEOUT))
        .build()
        .into()
}

fn random_token() -> Result<String, String> {
    let mut buf = [0u8; 32];
    getrandom::fill(&mut buf).map_err(|e| e.to_string())?;
    Ok(URL_SAFE_NO_PAD.encode(buf))
}

fn challenge(verifier: &str) -> String {
    URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()))
}

fn endpoint(server: &str, path: &str) -> String {
    format!("{}{path}", server.trim_end_matches('/'))
}

/// A server from before OAuth has no metadata; its panel signs in by itself.
fn supports_oauth(agent: &ureq::Agent, server: &str) -> Result<bool, String> {
    let res = agent
        .get(endpoint(server, "/.well-known/oauth-authorization-server"))
        .call()
        .map_err(|e| e.to_string())?;
    Ok(res.status().is_success())
}

fn read_tokens(mut res: ureq::http::Response<ureq::Body>) -> Result<TokenResponse, String> {
    if !res.status().is_success() {
        return Err(format!("the server refused the sign-in ({})", res.status()));
    }
    res.body_mut().read_json().map_err(|e| e.to_string())
}

/// `Ok(None)` means the server no longer accepts the token. An outage is an error instead, because
/// it is no reason to sign in again or to forget the session.
fn refresh(agent: &ureq::Agent, server: &str, token: &str) -> Result<Option<TokenResponse>, String> {
    let res = agent
        .post(endpoint(server, "/oauth/token"))
        .send_form([("grant_type", "refresh_token"), ("refresh_token", token), ("client_id", CLIENT_ID)])
        .map_err(|e| e.to_string())?;
    match res.status().as_u16() {
        400 | 401 | 404 => Ok(None),
        _ => read_tokens(res).map(Some),
    }
}

fn query_value(url: &Url, key: &str) -> Option<String> {
    url.query_pairs().find(|(k, _)| k == key).map(|(_, v)| v.into_owned())
}

fn answer(mut stream: TcpStream, text: &str) {
    let body = format!(
        "<!doctype html><meta charset=utf-8><title>Runner Deck</title>\
         <body style=\"font:16px system-ui;text-align:center;padding:64px\">{text}</body>"
    );
    let _ = write!(
        stream,
        "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
}

/// Waits for the browser to come back with the code. Anything else that connects, such as another
/// page probing the port, is answered and ignored, because it cannot know the state value.
fn wait_for_code(listener: &TcpListener, port: u16, state: &str, wait: Duration, done_text: &str) -> Result<String, String> {
    listener.set_nonblocking(true).map_err(|e| e.to_string())?;
    let started = Instant::now();
    loop {
        let stream = match listener.accept() {
            Ok((stream, _)) => stream,
            Err(e) if e.kind() == ErrorKind::WouldBlock => {
                if started.elapsed() >= wait {
                    return Err("the sign-in timed out".to_string());
                }
                std::thread::sleep(Duration::from_millis(100));
                continue;
            }
            Err(e) => return Err(e.to_string()),
        };
        let _ = stream.set_nonblocking(false);
        let _ = stream.set_read_timeout(Some(Duration::from_secs(5)));
        let mut line = String::new();
        if BufReader::new(&stream).read_line(&mut line).is_err() {
            continue;
        }
        let path = line.split_whitespace().nth(1).unwrap_or("/");
        let Ok(url) = Url::parse(&format!("http://127.0.0.1:{port}{path}")) else { continue };
        if url.path() != "/callback" || query_value(&url, "state").as_deref() != Some(state) {
            answer(stream, "");
            continue;
        }
        if let Some(error) = query_value(&url, "error") {
            answer(stream, done_text);
            return Err(format!("the sign-in was not completed ({error})"));
        }
        answer(stream, done_text);
        return query_value(&url, "code").ok_or_else(|| "no code in the answer".to_string());
    }
}

fn login(
    agent: &ureq::Agent,
    server: &str,
    open_browser: &dyn Fn(&Url),
    wait: Duration,
    done_text: &str,
) -> Result<TokenResponse, String> {
    let listener = TcpListener::bind(("127.0.0.1", 0)).map_err(|e| e.to_string())?;
    let port = listener.local_addr().map_err(|e| e.to_string())?.port();
    let redirect = format!("http://127.0.0.1:{port}/callback");
    let (verifier, state) = (random_token()?, random_token()?);
    let mut authorize = Url::parse(&endpoint(server, "/oauth/authorize")).map_err(|e| e.to_string())?;
    authorize
        .query_pairs_mut()
        .append_pair("client_id", CLIENT_ID)
        .append_pair("redirect_uri", &redirect)
        .append_pair("response_type", "code")
        .append_pair("code_challenge", &challenge(&verifier))
        .append_pair("code_challenge_method", "S256")
        .append_pair("state", &state);
    open_browser(&authorize);
    let code = wait_for_code(&listener, port, &state, wait, done_text)?;
    let res = agent
        .post(endpoint(server, "/oauth/token"))
        .send_form([
            ("grant_type", "authorization_code"),
            ("code", code.as_str()),
            ("code_verifier", verifier.as_str()),
            ("client_id", CLIENT_ID),
            ("redirect_uri", redirect.as_str()),
        ])
        .map_err(|e| e.to_string())?;
    read_tokens(res)
}

fn ticket_url(agent: &ureq::Agent, server: &str, access: &str) -> Result<String, String> {
    let mut res = agent
        .post(endpoint(server, "/oauth/ticket"))
        .header("Authorization", &format!("Bearer {access}"))
        .send_empty()
        .map_err(|e| e.to_string())?;
    if !res.status().is_success() {
        return Err(format!("the server refused the sign-in ({})", res.status()));
    }
    let ticket: TicketResponse = res.body_mut().read_json().map_err(|e| e.to_string())?;
    Ok(ticket.url)
}

/// Prepares a signed-in panel address, signing in through the browser when the saved session is gone.
pub fn open(server: &str, saved_refresh: Option<&str>, open_browser: &dyn Fn(&Url), done_text: &str) -> Result<Opened, String> {
    open_with_wait(server, saved_refresh, open_browser, LOGIN_WAIT, done_text)
}

fn open_with_wait(
    server: &str,
    saved_refresh: Option<&str>,
    open_browser: &dyn Fn(&Url),
    wait: Duration,
    done_text: &str,
) -> Result<Opened, String> {
    let agent = agent();
    let saved = match saved_refresh {
        Some(token) => refresh(&agent, server, token)?,
        None => None,
    };
    let tokens = match saved {
        Some(tokens) => tokens,
        None => {
            if !supports_oauth(&agent, server)? {
                return Ok(Opened { url: server.to_string(), refresh_token: None });
            }
            login(&agent, server, open_browser, wait, done_text)?
        }
    };
    let url = ticket_url(&agent, server, &tokens.access_token)?;
    Ok(Opened { url, refresh_token: Some(tokens.refresh_token) })
}

/// Ends the session on the server. A failure leaves it to expire there.
pub fn revoke(server: &str, token: &str) {
    let _ = agent().post(endpoint(server, "/oauth/revoke")).send_form([("token", token)]);
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::process::{Child, Command, Stdio};

    #[test]
    fn challenge_matches_the_rfc_example() {
        assert_eq!(challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"), "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM");
    }

    #[test]
    fn the_listener_ignores_answers_without_the_state() {
        let listener = TcpListener::bind(("127.0.0.1", 0)).unwrap();
        let port = listener.local_addr().unwrap().port();
        let client = std::thread::spawn(move || {
            for path in ["/favicon.ico", "/callback?code=stolen&state=wrong", "/callback?code=real&state=good"] {
                let mut s = TcpStream::connect(("127.0.0.1", port)).unwrap();
                write!(s, "GET {path} HTTP/1.1\r\nHost: x\r\n\r\n").unwrap();
                let mut reply = String::new();
                let _ = std::io::Read::read_to_string(&mut s, &mut reply);
            }
        });
        let code = wait_for_code(&listener, port, "good", Duration::from_secs(10), "done").unwrap();
        client.join().unwrap();
        assert_eq!(code, "real");
    }

    #[test]
    fn the_listener_reports_a_refusal_and_gives_up() {
        let listener = TcpListener::bind(("127.0.0.1", 0)).unwrap();
        let port = listener.local_addr().unwrap().port();
        let client = std::thread::spawn(move || {
            let mut s = TcpStream::connect(("127.0.0.1", port)).unwrap();
            write!(s, "GET /callback?error=access_denied&state=good HTTP/1.1\r\n\r\n").unwrap();
            let mut reply = String::new();
            let _ = std::io::Read::read_to_string(&mut s, &mut reply);
        });
        let err = wait_for_code(&listener, port, "good", Duration::from_secs(10), "done").unwrap_err();
        client.join().unwrap();
        assert!(err.contains("access_denied"), "{err}");
        let err = wait_for_code(&listener, port, "good", Duration::from_millis(10), "done").unwrap_err();
        assert!(err.contains("timed out"), "{err}");
    }

    struct TestManager {
        child: Child,
        url: String,
        dir: std::path::PathBuf,
    }

    impl Drop for TestManager {
        fn drop(&mut self) {
            let _ = self.child.kill();
            let _ = self.child.wait();
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    fn start_manager() -> TestManager {
        let port = TcpListener::bind(("127.0.0.1", 0)).unwrap().local_addr().unwrap().port();
        let dir = std::env::temp_dir().join(format!("rd-oauth-test-{port}"));
        std::fs::create_dir_all(dir.join("config")).unwrap();
        std::fs::write(dir.join("config/config.yaml"), format!("runners:\n  base_path: {}\n  items: []\n", dir.join("runners").display())).unwrap();
        let binary = std::env::var("RUNNER_MANAGER_BIN").expect("RUNNER_MANAGER_BIN");
        let child = Command::new(binary)
            .current_dir(&dir)
            .args(["-config", "config/config.yaml"])
            .env("BASIC_AUTH_USER", "admin")
            .env("BASIC_AUTH_PASSWORD", "testpw")
            .env("SERVER_ADDR", "127.0.0.1")
            .env("SERVER_PORT", port.to_string())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let started = Instant::now();
        while TcpStream::connect(("127.0.0.1", port)).is_err() {
            assert!(started.elapsed() < Duration::from_secs(10), "manager did not start");
            std::thread::sleep(Duration::from_millis(50));
        }
        TestManager { child, url: format!("http://127.0.0.1:{port}"), dir }
    }

    /// Plays the person in the browser: signs in, approves, and follows the redirect home.
    fn browser_approves(authorize: &Url, server: &str) {
        let agent = agent();
        let res = agent
            .post(endpoint(server, "/login"))
            .send_form([("username", "admin"), ("password", "testpw")])
            .unwrap();
        let cookie = res.headers().get("set-cookie").unwrap().to_str().unwrap().split(';').next().unwrap().to_string();
        let mut form: Vec<(String, String)> = authorize.query_pairs().into_owned().collect();
        form.push(("decision".into(), "approve".into()));
        let res = agent
            .post(endpoint(server, "/oauth/authorize"))
            .header("Cookie", &cookie)
            .send_form(form.iter().map(|(k, v)| (k.as_str(), v.as_str())))
            .unwrap();
        assert_eq!(res.status(), 303);
        let back = res.headers().get("location").unwrap().to_str().unwrap().to_string();
        std::thread::spawn(move || {
            let _ = agent.get(back).call();
        });
    }

    // Needs a built Manager: RUNNER_MANAGER_BIN=... cargo test -- --ignored
    #[test]
    #[ignore]
    fn signs_in_keeps_the_session_and_recovers_when_it_ends() {
        let manager = start_manager();
        let server = manager.url.clone();
        let opens = std::cell::Cell::new(0);
        let approve = |url: &Url| {
            opens.set(opens.get() + 1);
            browser_approves(url, &server);
        };

        let first = open_with_wait(&server, None, &approve, Duration::from_secs(20), "done").unwrap();
        assert_eq!(opens.get(), 1);
        assert!(first.url.contains("/login/ticket?t="), "{}", first.url);
        let token = first.refresh_token.unwrap();

        let res = agent().get(&first.url).call().unwrap();
        assert_eq!(res.status(), 303);
        assert!(res.headers().get("set-cookie").is_some(), "the ticket must sign the window in");

        // A saved session needs no browser, and its refresh token rotates.
        let second = open_with_wait(&server, Some(&token), &approve, Duration::from_secs(20), "done").unwrap();
        assert_eq!(opens.get(), 1);
        let rotated = second.refresh_token.unwrap();
        assert_ne!(rotated, token);

        // Once ended on the server, opening signs in again.
        revoke(&server, &rotated);
        let third = open_with_wait(&server, Some(&rotated), &approve, Duration::from_secs(20), "done").unwrap();
        assert_eq!(opens.get(), 2);
        assert!(third.refresh_token.is_some());
    }

    #[test]
    fn an_outage_is_not_a_lost_session() {
        let port = TcpListener::bind(("127.0.0.1", 0)).unwrap().local_addr().unwrap().port();
        let never = |_: &Url| panic!("an outage must not open the browser");
        assert!(open_with_wait(&format!("http://127.0.0.1:{port}"), Some("a.b"), &never, Duration::from_millis(50), "done").is_err());
    }

    #[test]
    #[ignore]
    fn a_refused_sign_in_is_an_error() {
        let manager = start_manager();
        let server = manager.url.clone();
        let deny = |url: &Url| {
            let back = query_value(url, "redirect_uri").unwrap();
            let state = query_value(url, "state").unwrap();
            std::thread::spawn(move || {
                let _ = agent().get(format!("{back}?error=access_denied&state={state}")).call();
            });
        };
        let err = open_with_wait(&server, None, &deny, Duration::from_secs(20), "done").err().unwrap();
        assert!(err.contains("access_denied"), "{err}");
    }
}
