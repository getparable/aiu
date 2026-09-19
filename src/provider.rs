use anyhow::{Context, Result, anyhow, bail};
use base64::Engine;
use rand::{Rng, distributions::Alphanumeric};
use reqwest::blocking::{Client, Response};
use reqwest::redirect::Policy;
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};
use std::io::{Read, Write};
use std::net::TcpListener;
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
    mpsc::{self, Receiver},
};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use url::Url;

use crate::config::Config;
use crate::model::{Provider, Record};

const CLAUDE_CLIENT: &str = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";
const CODEX_CLIENT: &str = "app_EMoamEEZ73f0CkXaXp7hrann";
const MAX_BODY: usize = 4 * 1024 * 1024;

#[derive(Clone)]
pub struct Api {
    config: Config,
    client: Client,
}

pub struct ApiResponse {
    pub status: u16,
    pub body: Value,
    pub retry_after: Option<u64>,
}

/// A cloneable cancellation signal for a browser login wait.
pub type LoginCancellation = Arc<AtomicBool>;

#[derive(Debug)]
pub struct LoginRejected(String);
impl std::fmt::Display for LoginRejected {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}
impl std::error::Error for LoginRejected {}

pub struct LoginSession {
    pub authorize_url: String,
    pub manual: bool,
    provider: Provider,
    verifier: String,
    state: String,
    redirect_uri: String,
    scopes: String,
    receiver: Option<Receiver<Result<String>>>,
    cancel: Option<Arc<AtomicBool>>,
}

impl Api {
    pub fn new(config: Config) -> Result<Self> {
        let client = Client::builder()
            .redirect(Policy::none())
            .timeout(Duration::from_secs(20))
            .user_agent(concat!("aiu-rs/", env!("CARGO_PKG_VERSION")))
            .default_headers({
                let mut headers = reqwest::header::HeaderMap::new();
                headers.insert(
                    reqwest::header::ACCEPT,
                    reqwest::header::HeaderValue::from_static("application/json"),
                );
                headers
            })
            .build()?;
        Ok(Self { config, client })
    }

    fn response(&self, response: Response) -> Result<ApiResponse> {
        let status = response.status().as_u16();
        let retry_after = response
            .headers()
            .get(reqwest::header::RETRY_AFTER)
            .and_then(|v| v.to_str().ok())
            .and_then(parse_retry_after);
        let mut bytes = Vec::new();
        response
            .take((MAX_BODY + 1) as u64)
            .read_to_end(&mut bytes)?;
        if bytes.len() > MAX_BODY {
            bail!("response exceeds 4 MiB limit")
        }
        let body = serde_json::from_slice(&bytes)
            .unwrap_or_else(|_| Value::String(String::from_utf8_lossy(&bytes).into_owned()));
        Ok(ApiResponse {
            status,
            body,
            retry_after,
        })
    }

    fn error_response(&self, action: &str, r: &ApiResponse) -> anyhow::Error {
        let detail = r
            .body
            .get("error_description")
            .and_then(Value::as_str)
            .or_else(|| {
                r.body
                    .get("error")
                    .and_then(|x| x.get("message"))
                    .and_then(Value::as_str)
            })
            .or_else(|| r.body.get("error").and_then(Value::as_str))
            .unwrap_or("request failed");
        let reason = if detail.contains("invalid_grant") {
            "invalid_grant; sign in again"
        } else if r.status == 401 || r.status == 403 {
            "login rejected; sign in again"
        } else {
            "provider rejected the request"
        };
        let message = format!("{action} failed (HTTP {}): {reason}", r.status);
        if detail.contains("invalid_grant")
            || (action == "token refresh" && [400, 401, 403].contains(&r.status))
        {
            LoginRejected(message).into()
        } else {
            anyhow!(message)
        }
    }

    pub fn profile(&self, token: &str) -> Result<Map<String, Value>> {
        let r = self
            .client
            .get(&self.config.claude_profile_url)
            .header("Authorization", format!("Bearer {token}"))
            .header("anthropic-beta", "oauth-2025-04-20")
            .header("Accept", "application/json")
            .header("User-Agent", "aiu-rs/0.1")
            .send()
            .map_err(|e| anyhow!("profile request failed: {}", redact(&e.to_string())))?;
        let a = self.response(r)?;
        if !(200..300).contains(&a.status) {
            return Err(self.error_response("profile", &a));
        }
        let root = a
            .body
            .as_object()
            .context("profile response was not an object")?;
        let account = root
            .get("account")
            .and_then(Value::as_object)
            .unwrap_or(root);
        let org = root.get("organization").and_then(Value::as_object);
        let email = ["email", "emailAddress", "email_address"]
            .iter()
            .find_map(|k| account.get(*k).and_then(Value::as_str))
            .unwrap_or("");
        if email.is_empty() {
            bail!("profile response did not include an email")
        }
        let mut out = Map::new();
        out.insert("emailAddress".into(), Value::String(email.into()));
        for (out_key, src, obj) in [
            ("accountUuid", "uuid", account),
            ("displayName", "display_name", account),
            ("fullName", "full_name", account),
            ("organizationUuid", "uuid", org.unwrap_or(&Map::new())),
            ("organizationName", "name", org.unwrap_or(&Map::new())),
            (
                "organizationType",
                "organization_type",
                org.unwrap_or(&Map::new()),
            ),
            ("billingType", "billing_type", org.unwrap_or(&Map::new())),
            (
                "organizationRateLimitTier",
                "rate_limit_tier",
                org.unwrap_or(&Map::new()),
            ),
        ] {
            if let Some(v) = obj.get(src) {
                out.insert(out_key.into(), v.clone());
            }
        }
        Ok(out)
    }

    pub fn refresh(&self, previous: &Record) -> Result<Record> {
        if previous.refresh_token.is_empty() {
            bail!("cannot refresh without a refresh token")
        }
        let url = if previous.provider == Provider::Claude {
            let mut last = None;
            for u in &self.config.claude_token_urls {
                let r = self.client.post(u).json(&serde_json::json!({"grant_type":"refresh_token","refresh_token":previous.refresh_token,"client_id":CLAUDE_CLIENT})).send();
                match r {
                    Ok(resp) => {
                        let a = self.response(resp)?;
                        if (200..300).contains(&a.status) {
                            return self.tokens_record(previous, a.body);
                        }
                        last = Some(self.error_response("token refresh", &a));
                        if a.status == 400 || a.status == 401 {
                            break;
                        }
                    }
                    Err(e) => {
                        last = Some(anyhow!(
                            "token refresh request failed: {}",
                            redact(&e.to_string())
                        ))
                    }
                }
            }
            return Err(last.unwrap_or_else(|| anyhow!("token refresh failed")));
        } else {
            &self.config.codex_token_url
        };
        let payload = serde_json::json!({"grant_type":"refresh_token", "refresh_token":previous.refresh_token, "client_id":CODEX_CLIENT});
        let a = self.response(
            self.client
                .post(url)
                .json(&payload)
                .header("Accept", "application/json")
                .header("User-Agent", "aiu-rs/0.1")
                .send()?,
        )?;
        if !(200..300).contains(&a.status) {
            return Err(self.error_response("token refresh", &a));
        }
        self.tokens_record(previous, a.body)
    }

    fn tokens_record(&self, previous: &Record, body: Value) -> Result<Record> {
        let o = body
            .as_object()
            .context("token response was not a JSON object")?;
        let now = now_ms();
        let mut r = previous.clone();
        r.access_token = o
            .get("access_token")
            .and_then(Value::as_str)
            .filter(|v| !v.is_empty())
            .context("token response did not include an access token")?
            .into();
        if let Some(v) = o
            .get("refresh_token")
            .and_then(Value::as_str)
            .filter(|v| !v.is_empty())
        {
            r.refresh_token = v.into();
        }
        if let Some(v) = o
            .get("id_token")
            .and_then(Value::as_str)
            .filter(|v| !v.is_empty())
        {
            r.id_token = v.into();
        }
        r.last_refresh = now;
        if let Some(scope) = o.get("scope").and_then(Value::as_str) {
            r.scopes = scope.split_whitespace().map(str::to_owned).collect();
        }
        r.expires_at = o
            .get("expires_in")
            .and_then(Value::as_i64)
            .filter(|n| *n > 0)
            .map(|n| now.saturating_add(n.saturating_mul(1000)))
            .unwrap_or(0);
        if let Some(n) = o
            .get("refresh_token_expires_in")
            .and_then(Value::as_i64)
            .filter(|n| *n > 0)
        {
            r.refresh_token_expires_at = now.saturating_add(n.saturating_mul(1000));
        }
        if r.provider == Provider::Codex {
            r.expires_at = jwt_expiry(&r.access_token)
                .or_else(|| (r.expires_at > 0).then_some(r.expires_at))
                .unwrap_or(now + 8 * 24 * 60 * 60 * 1000);
            let claims = jwt_claims(&r.id_token);
            let auth = claims
                .get("https://api.openai.com/auth")
                .and_then(Value::as_object);
            let profile = claims
                .get("https://api.openai.com/profile")
                .and_then(Value::as_object);
            if r.email.is_empty() {
                r.email = claims
                    .get("email")
                    .and_then(Value::as_str)
                    .or_else(|| profile.and_then(|x| x.get("email")).and_then(Value::as_str))
                    .unwrap_or("")
                    .into();
            }
            if r.account_id.is_empty() {
                r.account_id = auth
                    .and_then(|x| x.get("chatgpt_account_id"))
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .into();
            }
            if r.plan_type.is_empty() {
                r.plan_type = auth
                    .and_then(|x| x.get("chatgpt_plan_type"))
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .into();
            }
            if r.user_id.is_empty() {
                r.user_id = auth
                    .and_then(|x| x.get("chatgpt_user_id"))
                    .and_then(Value::as_str)
                    .or_else(|| auth.and_then(|x| x.get("user_id")).and_then(Value::as_str))
                    .unwrap_or("")
                    .into();
            }
        }
        if r.provider == Provider::Claude && r.expires_at == 0 {
            r.expires_at = now + 3_600_000;
        }
        Ok(r)
    }

    pub fn usage(&self, record: &Record) -> Result<ApiResponse> {
        let url = if record.provider == Provider::Claude {
            &self.config.claude_usage_url
        } else {
            &self.config.codex_usage_url
        };
        let request = self
            .client
            .get(url)
            .header("Authorization", format!("Bearer {}", record.access_token));
        let request = match record.provider {
            Provider::Claude => request.header("anthropic-beta", "oauth-2025-04-20"),
            Provider::Codex => request.header("ChatGPT-Account-Id", &record.account_id),
        };
        let r = request
            .send()
            .map_err(|e| anyhow!("usage request failed: {}", redact(&e.to_string())))?;
        let a = self.response(r)?;
        if (200..300).contains(&a.status) && !a.body.is_object() && !a.body.is_array() {
            bail!("usage response was not JSON")
        }
        Ok(a)
    }

    pub fn begin_login(
        &self,
        provider: Provider,
        read_only: bool,
        manual: bool,
        console: bool,
    ) -> Result<LoginSession> {
        if provider == Provider::Codex && manual {
            bail!("Codex login does not support manual code entry")
        }
        let verifier: String = rand::thread_rng()
            .sample_iter(&Alphanumeric)
            .take(64)
            .map(char::from)
            .collect();
        let state: String = rand::thread_rng()
            .sample_iter(&Alphanumeric)
            .take(32)
            .map(char::from)
            .collect();
        let challenge = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .encode(Sha256::digest(verifier.as_bytes()));
        let (base, path, redirect, rx, cancel, scopes) = if provider == Provider::Codex {
            let (p, rx, cancel) = bind_callback(1455, "/auth/callback", state.clone())
                .or_else(|_| bind_callback(1457, "/auth/callback", state.clone()))?;
            (
                self.config.codex_authorize_url.clone(),
                "/auth/callback",
                format!("http://localhost:{p}/auth/callback"),
                Some(rx),
                Some(cancel),
                "openid profile email offline_access api.connectors.read api.connectors.invoke"
                    .to_owned(),
            )
        } else if manual {
            (
                if console {
                    self.config.claude_authorize_console_url.clone()
                } else {
                    self.config.claude_authorize_url.clone()
                },
                "/callback",
                self.config.claude_manual_redirect_url.clone(),
                None,
                None,
                if read_only {
                    "user:profile".to_owned()
                } else {
                    "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload".to_owned()
                },
            )
        } else {
            let (p, rx, cancel) = bind_callback(54545, "/callback", state.clone())
                .or_else(|_| bind_callback(0, "/callback", state.clone()))?;
            (
                if console {
                    self.config.claude_authorize_console_url.clone()
                } else {
                    self.config.claude_authorize_url.clone()
                },
                "/callback",
                format!("http://localhost:{p}/callback"),
                Some(rx),
                Some(cancel),
                if read_only {
                    "user:profile".to_owned()
                } else {
                    "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload".to_owned()
                },
            )
        };
        let mut u = Url::parse(&base)?;
        {
            let mut q = u.query_pairs_mut();
            q.append_pair("response_type", "code")
                .append_pair(
                    "client_id",
                    if provider == Provider::Codex {
                        CODEX_CLIENT
                    } else {
                        CLAUDE_CLIENT
                    },
                )
                .append_pair("redirect_uri", &redirect)
                .append_pair("code_challenge", &challenge)
                .append_pair("code_challenge_method", "S256")
                .append_pair("state", &state);
            if provider == Provider::Claude {
                q.append_pair("code", "true").append_pair("scope", &scopes);
            } else {
                q.append_pair(
                    "scope",
                    "openid profile email offline_access api.connectors.read api.connectors.invoke",
                )
                .append_pair("originator", "codex_cli_rs")
                .append_pair("codex_cli_simplified_flow", "true")
                .append_pair("id_token_add_organizations", "true");
            }
        }
        let _ = path;
        Ok(LoginSession {
            authorize_url: u.to_string(),
            manual,
            provider,
            verifier,
            state,
            redirect_uri: redirect,
            scopes,
            receiver: rx,
            cancel,
        })
    }

    pub fn complete_login(&self, session: &LoginSession, code: &str) -> Result<Record> {
        let mut code = code.trim().to_string();
        if session.provider == Provider::Claude {
            let p = code.split_once('#');
            if let Some((c, s)) = p {
                if subtle::ConstantTimeEq::ct_eq(s.as_bytes(), session.state.as_bytes()).unwrap_u8()
                    != 1
                {
                    bail!("authorization state mismatch")
                }
                code = c.into();
            }
        }
        if code.is_empty() {
            bail!("no authorization code to exchange")
        }
        let body = if session.provider == Provider::Codex {
            self.response(
                self.client
                    .post(&self.config.codex_token_url)
                    .form(&[
                        ("grant_type", "authorization_code"),
                        ("code", code.as_str()),
                        ("redirect_uri", session.redirect_uri.as_str()),
                        ("client_id", CODEX_CLIENT),
                        ("code_verifier", session.verifier.as_str()),
                    ])
                    .header("Accept", "application/json")
                    .header("User-Agent", "aiu-rs/0.1")
                    .send()?,
            )?
        } else {
            let mut last = None;
            let mut last_transport = None;
            for u in &self.config.claude_token_urls {
                let response = self.client.post(u).json(&serde_json::json!({"grant_type":"authorization_code","code":code,"state":session.state,"redirect_uri":session.redirect_uri,"client_id":CLAUDE_CLIENT,"code_verifier":session.verifier})).send();
                let a = match response {
                    Ok(response) => self.response(response)?,
                    Err(error) => {
                        last_transport = Some(error);
                        continue;
                    }
                };
                if (200..300).contains(&a.status) {
                    last = Some(a);
                    break;
                }
                let stop = a.status == 400 || a.status == 401;
                last = Some(a);
                if stop {
                    break;
                }
            }
            last.ok_or_else(|| {
                anyhow!(
                    "code exchange unavailable{}",
                    last_transport
                        .map(|e| format!(": {}", redact(&e.to_string())))
                        .unwrap_or_default()
                )
            })?
        };
        if !(200..300).contains(&body.status) {
            return Err(self.error_response("code exchange", &body));
        }
        let mut r = Record {
            provider: session.provider,
            ..Record::default()
        };
        r = self.tokens_record(&r, body.body)?;
        if r.scopes.is_empty() {
            r.scopes = session
                .scopes
                .split_whitespace()
                .map(str::to_owned)
                .collect();
        }
        Ok(r)
    }

    pub fn open_browser(&self, url: &str) -> Result<()> {
        let u = Url::parse(url)?;
        if u.scheme() != "https"
            || ![
                &self.config.claude_authorize_url,
                &self.config.claude_authorize_console_url,
                &self.config.codex_authorize_url,
            ]
            .iter()
            .any(|x| {
                Url::parse(x)
                    .ok()
                    .map(|a| {
                        a.scheme() == u.scheme()
                            && a.host_str() == u.host_str()
                            && a.port_or_known_default() == u.port_or_known_default()
                    })
                    .unwrap_or(false)
            })
        {
            bail!("refusing to open untrusted authorization URL")
        }
        webbrowser::open(url).map(|_| ()).map_err(Into::into)
    }
}

impl LoginSession {
    pub fn wait_for_code(&self) -> Result<String> {
        self.wait_for_code_cancellable(&Arc::new(AtomicBool::new(false)))
    }

    pub fn wait_for_code_cancellable(&self, cancellation: &LoginCancellation) -> Result<String> {
        if self.manual {
            bail!("manual login requires pasted code")
        }
        let receiver = self
            .receiver
            .as_ref()
            .context("login callback unavailable")?;
        loop {
            if cancellation.load(Ordering::Acquire) {
                if let Some(cancel) = &self.cancel {
                    cancel.store(true, Ordering::Release);
                }
                bail!("Sign-in cancelled")
            }
            match receiver.recv_timeout(Duration::from_millis(100)) {
                Ok(result) => return result,
                Err(mpsc::RecvTimeoutError::Timeout) => continue,
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    bail!("login callback unavailable")
                }
            }
        }
    }
}

fn bind_callback(
    port: u16,
    path: &'static str,
    state: String,
) -> Result<(u16, Receiver<Result<String>>, Arc<AtomicBool>)> {
    let listener = TcpListener::bind(("127.0.0.1", port))?;
    listener.set_nonblocking(true)?;
    let p = listener.local_addr()?.port();
    let (tx, rx) = mpsc::channel();
    let cancel = Arc::new(AtomicBool::new(false));
    let cancelled = cancel.clone();
    std::thread::spawn(move || {
        let deadline = Instant::now() + Duration::from_secs(300);
        loop {
            if cancelled.load(Ordering::Acquire) || Instant::now() >= deadline {
                let _ = tx.send(Err(anyhow!("timed out waiting for browser callback")));
                break;
            }
            match listener.accept() {
                Ok((mut s, _)) => {
                    let _ = s.set_read_timeout(Some(Duration::from_millis(100)));
                    let _ = s.set_write_timeout(Some(Duration::from_secs(3)));
                    let mut b = Vec::new();
                    let mut chunk = [0u8; 2048];
                    let read_deadline = Instant::now() + Duration::from_secs(10);
                    while b.len() < 16 * 1024
                        && Instant::now() < read_deadline
                        && !cancelled.load(Ordering::Acquire)
                    {
                        let n = match s.read(&mut chunk) {
                            Ok(n) => n,
                            Err(error)
                                if [
                                    std::io::ErrorKind::WouldBlock,
                                    std::io::ErrorKind::TimedOut,
                                ]
                                .contains(&error.kind()) =>
                            {
                                continue;
                            }
                            Err(_) => break,
                        };
                        if n == 0 {
                            break;
                        }
                        b.extend_from_slice(&chunk[..n]);
                        if b.windows(4).any(|w| w == b"\r\n\r\n") {
                            break;
                        }
                    }
                    let line = String::from_utf8_lossy(&b)
                        .lines()
                        .next()
                        .unwrap_or("")
                        .to_string();
                    let result = parse_callback(&line, path, &state);
                    let ok = result.is_ok();
                    let terminal_error = result.as_ref().err().is_some_and(|e| {
                        e.to_string().starts_with("authorization failed:")
                            || e.to_string() == "Sign-in cancelled"
                            || e.to_string() == "no authorization code"
                    });
                    let body = if ok {
                        "<h1>Login complete</h1>You may close this window."
                    } else {
                        "<h1>Login failed</h1>Start login again."
                    };
                    let resp = format!(
                        "HTTP/1.1 {}\r\nContent-Type: text/html; charset=utf-8\r\nCache-Control: no-store\r\nContent-Security-Policy: default-src 'none'\r\nReferrer-Policy: no-referrer\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                        if ok {
                            "200 OK"
                        } else if line.starts_with("GET ") {
                            "404 Not Found"
                        } else {
                            "405 Method Not Allowed"
                        },
                        body.len(),
                        body
                    );
                    let _ = s.write_all(resp.as_bytes());
                    if ok || terminal_error {
                        let _ = tx.send(result);
                        break;
                    }
                }
                Err(e)
                    if e.kind() == std::io::ErrorKind::WouldBlock && Instant::now() < deadline =>
                {
                    std::thread::sleep(Duration::from_millis(50))
                }
                Err(_) => {
                    let _ = tx.send(Err(anyhow!("timed out waiting for browser callback")));
                    break;
                }
            }
        }
    });
    Ok((p, rx, cancel))
}
fn parse_callback(line: &str, path: &str, state: &str) -> Result<String> {
    let target = line
        .strip_prefix("GET ")
        .and_then(|x| x.split_whitespace().next())
        .context("invalid callback")?;
    let u = Url::parse(&format!("http://localhost{target}"))?;
    let got = u
        .query_pairs()
        .find(|(k, _)| k == "state")
        .map(|(_, v)| v.into_owned())
        .unwrap_or_default();
    if u.path() != path
        || subtle::ConstantTimeEq::ct_eq(got.as_bytes(), state.as_bytes()).unwrap_u8() != 1
    {
        bail!("authorization state mismatch")
    }
    if let Some((_, e)) = u.query_pairs().find(|(k, _)| k == "error") {
        if e == "access_denied" {
            bail!("Sign-in cancelled")
        }
        bail!("authorization failed: {}", redact(&e))
    }
    u.query_pairs()
        .find(|(k, _)| k == "code")
        .map(|(_, v)| v.into_owned())
        .filter(|x| !x.is_empty())
        .context("no authorization code")
}

fn parse_retry_after(value: &str) -> Option<u64> {
    parse_retry_after_at(value, SystemTime::now())
}

fn parse_retry_after_at(value: &str, now: SystemTime) -> Option<u64> {
    let value = value.trim();
    if !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit()) {
        return Some(value.parse().unwrap_or(u64::MAX));
    }
    let deadline = httpdate::parse_http_date(value).ok()?;
    let delay = deadline.duration_since(now).unwrap_or_default();
    Some(
        delay
            .as_secs()
            .saturating_add(u64::from(delay.subsec_nanos() != 0)),
    )
}
fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64
}
fn jwt_expiry(token: &str) -> Option<i64> {
    let p = token.split('.').nth(1)?;
    let b = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(p)
        .ok()?;
    serde_json::from_slice::<Value>(&b)
        .ok()?
        .get("exp")?
        .as_i64()
        .map(|x| x.saturating_mul(1000))
}
fn jwt_claims(token: &str) -> Value {
    token
        .split('.')
        .nth(1)
        .and_then(|p| {
            base64::engine::general_purpose::URL_SAFE_NO_PAD
                .decode(p)
                .ok()
        })
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or(Value::Object(Map::new()))
}
pub fn redact(message: &str) -> String {
    use std::sync::OnceLock;
    static PATTERNS: OnceLock<Vec<regex::Regex>> = OnceLock::new();
    let patterns = PATTERNS.get_or_init(|| [
        r#"(?i)\bbearer\s+[^\s\"',;}]+"#,
        r#"(?i)(?:access_?token|refresh_?token|id_?token|authorization_?code|code_?verifier)[\"']?\s*[:=]\s*(?:\"[^\"]*\"|'[^']*'|[^\s&,;}]+)"#,
        r"\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*",
        r"\b(?:sk-ant-|sk-|rt_)[A-Za-z0-9_-]{8,}",
    ].iter().map(|pattern| regex::Regex::new(pattern).expect("constant redaction regex")).collect());
    let mut text = message.replace(['\r', '\n'], " ");
    for pattern in patterns {
        text = pattern.replace_all(&text, "[REDACTED]").into_owned();
    }
    text.chars().take(500).collect()
}
impl Drop for LoginSession {
    fn drop(&mut self) {
        if let Some(c) = &self.cancel {
            c.store(true, Ordering::Release);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    #[test]
    fn redaction_handles_bearer_jwt_unicode_and_repeated_fields() {
        let s = redact(
            "Bearer abcdefghijklmnopqrstuvwxyz.abcdefghijklmnopqrstuvwxyz.abcdefghijklmnopqrstuvwxyz access_token=one refresh_token:two id_token=three — café",
        );
        assert!(!s.contains("abcdefghijklmnopqrstuvwxyz"));
        assert!(!s.contains("one"));
        assert!(!s.contains("two"));
        assert!(!s.contains("three"));
        assert!(s.contains("café"));
    }

    #[test]
    fn token_response_preserves_metadata_and_uses_scope_and_expiry() {
        let cfg = Config::for_home(PathBuf::from("."), PathBuf::from("."));
        let api = Api::new(cfg).unwrap();
        let old = Record {
            provider: Provider::Claude,
            email: "keep@example.test".into(),
            label: "label".into(),
            refresh_token: "old".into(),
            ..Default::default()
        };
        let r=api.tokens_record(&old,serde_json::json!({"access_token":"new","scope":"user:profile user:inference","expires_in":12})).unwrap();
        assert_eq!(r.email, "keep@example.test");
        assert_eq!(r.label, "label");
        assert_eq!(r.access_token, "new");
        assert_eq!(r.scopes, vec!["user:profile", "user:inference"]);
        assert!(r.expires_at > now_ms());
        assert!(api.tokens_record(&old, serde_json::json!({})).is_err());
    }

    #[test]
    fn callback_rejects_mismatch_without_exposing_code() {
        assert!(
            parse_callback(
                "GET /callback?code=secret&state=wrong HTTP/1.1",
                "/callback",
                "right"
            )
            .is_err()
        );
        assert_eq!(
            parse_callback(
                "GET /callback?code=ok&state=right HTTP/1.1",
                "/callback",
                "right"
            )
            .unwrap(),
            "ok"
        );
        assert!(
            parse_callback(
                "GET /callback?error=access_denied&state=wrong HTTP/1.1",
                "/callback",
                "right"
            )
            .is_err()
        );
        assert_eq!(
            parse_callback(
                "GET /callback?error=access_denied&state=right HTTP/1.1",
                "/callback",
                "right"
            )
            .unwrap_err()
            .to_string(),
            "Sign-in cancelled"
        );
    }

    #[test]
    fn retry_after_accepts_seconds_dates_and_safe_failures() {
        assert_eq!(parse_retry_after("17"), Some(17));
        assert_eq!(parse_retry_after("18446744073709551615"), Some(u64::MAX));
        assert_eq!(parse_retry_after("not-a-retry-value"), None);
        assert_eq!(parse_retry_after("Thu, 01 Jan 1970 00:00:00 GMT"), Some(0));
        assert_eq!(
            parse_retry_after("9999999999999999999999999"),
            Some(u64::MAX)
        );
        assert_eq!(parse_retry_after("+17"), None);
        let deadline = httpdate::parse_http_date("Sun, 06 Nov 1994 08:49:37 GMT").unwrap();
        let now = deadline - Duration::from_millis(7_200_500);
        for date in [
            "Sun, 06 Nov 1994 08:49:37 GMT",
            "Sunday, 06-Nov-94 08:49:37 GMT",
            "Sun Nov  6 08:49:37 1994",
        ] {
            assert_eq!(parse_retry_after_at(date, now), Some(7201));
            assert_eq!(
                parse_retry_after_at(date, deadline + Duration::from_secs(1)),
                Some(0)
            );
        }
    }

    #[test]
    fn listener_survives_wrong_state_and_consumes_valid_callback() {
        let (port, receiver, cancel) = bind_callback(0, "/callback", "expected".into()).unwrap();
        let request = |state: &str| {
            let mut stream = std::net::TcpStream::connect(("127.0.0.1", port)).unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            write!(
                stream,
                "GET /callback?code=private-code&state={state} HTTP/1.1\r\nHost: localhost\r\n\r\n"
            )
            .unwrap();
            let mut response = String::new();
            stream.read_to_string(&mut response).unwrap();
            assert!(!response.contains("private-code"));
            response
        };
        assert!(request("wrong").starts_with("HTTP/1.1 404"));
        assert!(request("expected").starts_with("HTTP/1.1 200"));
        assert_eq!(
            receiver
                .recv_timeout(Duration::from_secs(2))
                .unwrap()
                .unwrap(),
            "private-code"
        );
        cancel.store(true, Ordering::Release);
    }

    #[test]
    fn cancellation_releases_listener_with_incomplete_request() {
        let (port, receiver, cancel) = bind_callback(0, "/callback", "expected".into()).unwrap();
        let stream = std::net::TcpStream::connect(("127.0.0.1", port)).unwrap();
        std::thread::sleep(Duration::from_millis(100));
        cancel.store(true, Ordering::Release);
        assert!(
            receiver
                .recv_timeout(Duration::from_secs(2))
                .unwrap()
                .is_err()
        );
        // Receiving the cancellation result can precede the listener thread's teardown.
        drop(stream);
        let deadline = Instant::now() + Duration::from_secs(2);
        loop {
            if let Ok(listener) = std::net::TcpListener::bind(("127.0.0.1", port)) {
                drop(listener);
                break;
            }
            assert!(
                Instant::now() < deadline,
                "callback listener was not released"
            );
            std::thread::sleep(Duration::from_millis(25));
        }
    }

    #[test]
    fn cancellable_wait_returns_quickly_and_releases_listener_while_session_is_retained() {
        let (port, receiver, listener_cancel) =
            bind_callback(0, "/callback", "expected".into()).unwrap();
        let session = LoginSession {
            authorize_url: "https://example.test/authorize".into(),
            manual: false,
            provider: Provider::Claude,
            verifier: "verifier".into(),
            state: "expected".into(),
            redirect_uri: format!("http://localhost:{port}/callback"),
            scopes: "user:profile".into(),
            receiver: Some(receiver),
            cancel: Some(listener_cancel),
        };
        let cancellation = Arc::new(AtomicBool::new(false));
        let signal = cancellation.clone();
        let started = Instant::now();
        let signaler = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(100));
            signal.store(true, Ordering::Release);
        });
        let result = session.wait_for_code_cancellable(&cancellation);
        signaler.join().unwrap();
        assert_eq!(result.unwrap_err().to_string(), "Sign-in cancelled");
        assert!(started.elapsed() < Duration::from_secs(2));

        let deadline = Instant::now() + Duration::from_secs(2);
        loop {
            if let Ok(listener) = std::net::TcpListener::bind(("127.0.0.1", port)) {
                drop(listener);
                break;
            }
            assert!(
                Instant::now() < deadline,
                "callback listener was not released"
            );
            std::thread::sleep(Duration::from_millis(25));
        }
        drop(session);
    }

    #[test]
    fn browser_denial_is_terminal_only_with_matching_state() {
        let (port, receiver, cancel) = bind_callback(0, "/callback", "expected".into()).unwrap();
        let mut wrong = std::net::TcpStream::connect(("127.0.0.1", port)).unwrap();
        write!(
            wrong,
            "GET /callback?error=access_denied&state=wrong HTTP/1.1\r\nHost: localhost\r\n\r\n"
        )
        .unwrap();
        let mut response = String::new();
        wrong.read_to_string(&mut response).unwrap();
        assert!(response.starts_with("HTTP/1.1 404"));
        assert!(receiver.recv_timeout(Duration::from_millis(200)).is_err());

        let mut right = std::net::TcpStream::connect(("127.0.0.1", port)).unwrap();
        write!(
            right,
            "GET /callback?error=access_denied&state=expected HTTP/1.1\r\nHost: localhost\r\n\r\n"
        )
        .unwrap();
        let error = receiver
            .recv_timeout(Duration::from_secs(2))
            .unwrap()
            .unwrap_err();
        assert_eq!(error.to_string(), "Sign-in cancelled");
        cancel.store(true, Ordering::Release);
    }

    #[test]
    fn redact_opaque_bearer_and_camelcase_json_secrets() {
        let message = r#"Bearer opaque-secret {"accessToken": "access-secret", "refresh_token":"refresh-secret", "idToken":"id-secret"} access_token=other-secret"#;
        let redacted = redact(message);
        for secret in [
            "opaque-secret",
            "access-secret",
            "refresh-secret",
            "id-secret",
            "other-secret",
        ] {
            assert!(!redacted.contains(secret));
        }
        assert!(redact(&"é".repeat(600)).is_char_boundary(500));
    }
}
