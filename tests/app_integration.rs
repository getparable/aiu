use aiu::{
    app::App,
    config::Config,
    model::{Provider, Record},
    storage::Store,
};
use base64::Engine;
use serde_json::json;
use std::{
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    thread,
    time::Duration,
};
use tempfile::TempDir;

fn config(tmp: &TempDir) -> Config {
    let mut c = Config::for_home(tmp.path().into(), tmp.path().join("cfg"));
    c.store_mode = aiu::config::StoreMode::File;
    c.claude_use_keychain = false;
    c.claude_profile_url = "http://127.0.0.1:9/profile".into();
    c.claude_usage_url = "http://127.0.0.1:9/usage".into();
    c.claude_token_urls = vec!["http://127.0.0.1:9/token".into()];
    c.codex_usage_url = "http://127.0.0.1:9/usage".into();
    c.codex_token_url = "http://127.0.0.1:9/token".into();
    c
}
type MockResponse = (u16, String, Vec<(String, String)>);
struct ExpectedRequest {
    method: &'static str,
    path: &'static str,
    authorization: Option<&'static str>,
    headers: Vec<(&'static str, &'static str)>,
    body_contains: Vec<&'static str>,
}
fn server(responses: Vec<MockResponse>) -> (String, Arc<AtomicUsize>, thread::JoinHandle<()>) {
    server_checked(responses, Vec::new())
}
fn server_checked(
    responses: Vec<MockResponse>,
    expected: Vec<ExpectedRequest>,
) -> (String, Arc<AtomicUsize>, thread::JoinHandle<()>) {
    let server = tiny_http::Server::http("127.0.0.1:0").unwrap();
    let url = format!("http://{}", server.server_addr());
    let n = Arc::new(AtomicUsize::new(0));
    let nn = n.clone();
    let h = thread::spawn(move || {
        for (index, (status, body, headers)) in responses.into_iter().enumerate() {
            let mut request = server
                .recv_timeout(Duration::from_secs(5))
                .unwrap()
                .expect("expected mock request within five seconds");
            let mut request_body = Vec::new();
            request.as_reader().read_to_end(&mut request_body).unwrap();
            if let Some(expectation) = expected.get(index) {
                assert_eq!(request.method().as_str(), expectation.method);
                assert_eq!(request.url().split('?').next().unwrap(), expectation.path);
                let header = |name: &str| {
                    request
                        .headers()
                        .iter()
                        .find(|h| h.field.as_str().to_string().eq_ignore_ascii_case(name))
                        .map(|h| h.value.as_str())
                };
                assert_eq!(header("Authorization"), expectation.authorization);
                for (name, value) in &expectation.headers {
                    assert_eq!(header(name), Some(*value), "header {name}");
                }
                let body = String::from_utf8_lossy(&request_body);
                for fragment in &expectation.body_contains {
                    assert!(
                        body.contains(fragment),
                        "request body missing {fragment:?}: {body}"
                    );
                }
            }
            nn.fetch_add(1, Ordering::SeqCst);
            let mut response = tiny_http::Response::from_string(body)
                .with_status_code(status)
                .with_header(
                    tiny_http::Header::from_bytes("Content-Type", "application/json").unwrap(),
                );
            for (k, v) in headers {
                response.add_header(tiny_http::Header::from_bytes(k, v).unwrap());
            }
            request.respond(response).unwrap();
        }
    });
    (url, n, h)
}
fn record(p: Provider, email: &str) -> Record {
    Record {
        provider: p,
        email: email.into(),
        access_token: "access-secret".into(),
        refresh_token: "refresh-secret".into(),
        expires_at: chrono::Utc::now().timestamp_millis() + 3_600_000,
        ..Default::default()
    }
}
fn seed(c: &Config, rs: &[Record]) {
    let s = Store::new(c.clone());
    let mut idx = aiu::model::Index::default();
    for r in rs {
        s.set(r).unwrap();
        idx.accounts.push(aiu::model::IndexEntry {
            email: r.email.clone(),
            label: r.label.clone(),
            org: r.org_uuid.clone(),
            org_name: r.org_name.clone(),
            provider: r.provider,
            added_at: "now".into(),
        });
    }
    s.save_index(&idx).unwrap();
}
fn id_token(org: &str) -> String {
    let p=base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(format!(r#"{{"email":"same@example.test","https://api.openai.com/auth":{{"chatgpt_account_id":"{}"}}}}"#,org));
    format!("e30.{p}.sig")
}

#[test]
fn status_cache_shares_one_request_and_preserves_429_retry_after() {
    let t = TempDir::new().unwrap();
    let mut c = config(&t);
    let r = record(Provider::Claude, "cache@example.test");
    seed(&c, &[r]);
    let (u, n, h) = server(vec![(
        429,
        "{}".into(),
        vec![("Retry-After".into(), "3600".into())],
    )]);
    c.claude_usage_url = u;
    let a = App::new(c).unwrap();
    let s1 = a.status(Some(Provider::Claude), true).unwrap();
    let s2 = a.status(Some(Provider::Claude), true).unwrap();
    h.join().unwrap();
    assert_eq!(n.load(Ordering::SeqCst), 1);
    assert!(s1.accounts[0].error.contains("throttled"));
    assert!(s2.accounts[0].error.contains("throttled"));
}

#[test]
fn provider_filter_does_not_call_other_endpoint() {
    let t = TempDir::new().unwrap();
    let mut c = config(&t);
    let a1 = record(Provider::Claude, "c@example.test");
    let a2 = record(Provider::Codex, "x@example.test");
    seed(&c, &[a1, a2]);
    let (u, n, h) = server_checked(
        vec![(200, r#"{"five_hour":{"utilization":1}}"#.into(), vec![])],
        vec![ExpectedRequest {
            method: "GET",
            path: "/usage",
            authorization: Some("Bearer access-secret"),
            headers: vec![("anthropic-beta", "oauth-2025-04-20")],
            body_contains: vec![],
        }],
    );
    c.claude_usage_url = format!("{u}/usage");
    c.codex_usage_url = "http://127.0.0.1:9/unused".into();
    let app = App::new(c).unwrap();
    let out = app.status(Some(Provider::Claude), true).unwrap();
    h.join().unwrap();
    assert_eq!(out.accounts.len(), 1);
    assert_eq!(n.load(Ordering::SeqCst), 1);
}

#[test]
fn codex_usage_sends_account_header_and_bearer_token() {
    let t = TempDir::new().unwrap();
    let mut c = config(&t);
    let mut r = record(Provider::Codex, "codex@example.test");
    r.account_id = "org-123".into();
    seed(&c, &[r]);
    let (u, n, h) = server_checked(
        vec![(200, r#"{"five_hour":{"utilization":4}}"#.into(), vec![])],
        vec![ExpectedRequest {
            method: "GET",
            path: "/usage",
            authorization: Some("Bearer access-secret"),
            headers: vec![("ChatGPT-Account-Id", "org-123")],
            body_contains: vec![],
        }],
    );
    c.codex_usage_url = format!("{u}/usage");
    let out = App::new(c)
        .unwrap()
        .status(Some(Provider::Codex), true)
        .unwrap();
    h.join().unwrap();
    assert_eq!(out.accounts.len(), 1);
    assert_eq!(n.load(Ordering::SeqCst), 1);
}

#[test]
fn unauthorized_usage_refreshes_once_then_retries_and_views_contain_no_credentials() {
    let t = TempDir::new().unwrap();
    let mut c = config(&t);
    let r = record(Provider::Claude, "retry@example.test");
    seed(&c, &[r]);
    let (u,n,h)=server_checked(vec![(401,"{}".into(),vec![]),(200,r#"{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}"#.into(),vec![]),(200,r#"{"five_hour":{"utilization":2}}"#.into(),vec![])], vec![
        ExpectedRequest { method: "GET", path: "/usage", authorization: Some("Bearer access-secret"), headers: vec![("anthropic-beta", "oauth-2025-04-20")], body_contains: vec![] },
        ExpectedRequest { method: "POST", path: "/token", authorization: None, headers: vec![("Content-Type", "application/json")], body_contains: vec!["grant_type", "refresh_token", "refresh-secret", "client_id"] },
        ExpectedRequest { method: "GET", path: "/usage", authorization: Some("Bearer rotated-access"), headers: vec![("anthropic-beta", "oauth-2025-04-20")], body_contains: vec![] },
    ]);
    c.claude_usage_url = format!("{u}/usage");
    c.claude_token_urls = vec![format!("{u}/token")];
    let app = App::new(c).unwrap();
    let out = app.status(Some(Provider::Claude), true).unwrap();
    h.join().unwrap();
    assert_eq!(n.load(Ordering::SeqCst), 3);
    let text = serde_json::to_string(&out).unwrap();
    assert!(!text.contains("rotated-access"));
    assert!(!text.contains("refresh-secret"));
    assert_eq!(
        Store::new(app.config.clone())
            .get("claude:retry@example.test")
            .unwrap()
            .unwrap()
            .access_token,
        "rotated-access"
    );
}

#[test]
fn codex_same_email_different_orgs_keep_labels_and_remove_is_ambiguous() {
    let t = TempDir::new().unwrap();
    let c = config(&t);
    let app = App::new(c).unwrap();
    let mut a = record(Provider::Codex, "same@example.test");
    a.id_token = id_token("org-1");
    a.org_uuid = "org-1".into();
    let mut b = a.clone();
    b.org_uuid = "org-2".into();
    b.account_id = "org-2".into();
    b.id_token = id_token("org-2");
    assert!(app.save_login(a, Some("work")).is_ok());
    assert!(app.save_login(b, Some("personal")).is_ok());
    assert!(
        app.remove("same@example.test", Some(Provider::Codex))
            .is_err()
    );
    assert_eq!(app.list(Some(Provider::Codex)).unwrap().len(), 2);
}

#[test]
fn stale_claude_identity_does_not_adopt_unknown_live_token() {
    let t = TempDir::new().unwrap();
    let mut c = config(&t);
    let (u, _, h) = server(vec![
        (
            200,
            r#"{"account":{"email":"unknown@example.test"}}"#.into(),
            vec![],
        ),
        (200, r#"{"five_hour":{"utilization":1}}"#.into(), vec![]),
    ]);
    c.claude_profile_url = u.clone();
    c.claude_usage_url = u;
    std::fs::create_dir_all(&c.claude_dir).unwrap();
    std::fs::write(c.claude_dir.join(".credentials.json"),json!({"claudeAiOauth":{"accessToken":"unknown-live","refreshToken":"unknown-refresh","expiresAt":chrono::Utc::now().timestamp_millis()+3600000}}).to_string()).unwrap();
    let mut r = record(Provider::Claude, "tracked@example.test");
    r.access_token = "tracked-token".into();
    seed(&c, &[r]);
    let app = App::new(c).unwrap();
    let views = app.status(Some(Provider::Claude), false).unwrap();
    h.join().unwrap();
    assert_eq!(views.accounts[0].email, "tracked@example.test");
    assert_eq!(
        Store::new(app.config.clone())
            .get("claude:tracked@example.test")
            .unwrap()
            .unwrap()
            .access_token,
        "tracked-token"
    );
}

#[test]
fn cached_success_survives_throttling_and_is_shared_between_instances() {
    let temp = TempDir::new().unwrap();
    let mut cfg = config(&temp);
    let record = record(Provider::Claude, "stale@example.test");
    seed(&cfg, std::slice::from_ref(&record));
    let (url, count, worker) = server(vec![
        (
            200,
            json!({"five_hour":{"utilization":25},"seven_day":{"utilization":30}}).to_string(),
            vec![],
        ),
        (
            429,
            "{}".into(),
            vec![("Retry-After".into(), "7200".into())],
        ),
    ]);
    cfg.claude_usage_url = url;
    let first = App::new(cfg.clone())
        .unwrap()
        .status(Some(Provider::Claude), true)
        .unwrap();
    assert_eq!(first.accounts[0].windows[0].percent, 25.0);
    let second = App::new(cfg.clone())
        .unwrap()
        .status(Some(Provider::Claude), true)
        .unwrap();
    assert_eq!(first.accounts[0].fetched_at, second.accounts[0].fetched_at);
    assert_eq!(count.load(Ordering::SeqCst), 1);
    let mut cache: serde_json::Value = aiu::storage::read_json(&cfg.cache_file()).unwrap().unwrap();
    cache[record.key()]["attemptedAt"] = json!(chrono::Utc::now().timestamp_millis() - 301_000);
    aiu::storage::write_json(&cfg.cache_file(), &cache).unwrap();
    let stale = App::new(cfg.clone())
        .unwrap()
        .status(Some(Provider::Claude), true)
        .unwrap();
    assert_eq!(stale.accounts[0].windows[0].percent, 25.0);
    assert!(!stale.accounts[0].stale.is_empty());
    let again = App::new(cfg.clone())
        .unwrap()
        .status(Some(Provider::Claude), true)
        .unwrap();
    assert_eq!(again.accounts[0].stale, stale.accounts[0].stale);
    let cache: serde_json::Value = aiu::storage::read_json(&cfg.cache_file()).unwrap().unwrap();
    assert!(
        cache[record.key()]["limitedUntil"].as_i64().unwrap()
            > chrono::Utc::now().timestamp_millis() + 7_000_000,
        "cache: {cache}"
    );
    worker.join().unwrap();
    assert_eq!(count.load(Ordering::SeqCst), 2);
}

#[test]
fn rejected_refresh_remains_expired_even_with_cached_usage() {
    let temp = TempDir::new().unwrap();
    let mut cfg = config(&temp);
    let mut record = record(Provider::Claude, "expired@example.test");
    record.expires_at = 1;
    seed(&cfg, std::slice::from_ref(&record));
    aiu::storage::write_json(
        &cfg.cache_file(),
        &json!({record.key(): {
            "usage":{"seven_day":{"utilization":10}},"fetchedAt":1234,"attemptedAt":0
        }}),
    )
    .unwrap();
    let (url, count, worker) = server(vec![(401, "{}".into(), vec![])]);
    cfg.claude_token_urls = vec![url];
    for _ in 0..2 {
        let snapshot = App::new(cfg.clone())
            .unwrap()
            .status(Some(Provider::Claude), true)
            .unwrap();
        assert_eq!(snapshot.accounts[0].login.state, "expired");
        assert!(!snapshot.accounts[0].error.is_empty());
        assert!(!snapshot.accounts[0].recommended);
        assert!(snapshot.accounts[0].windows.is_empty());
    }
    worker.join().unwrap();
    assert_eq!(count.load(Ordering::SeqCst), 1);
}
