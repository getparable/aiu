use aiu::{
    live,
    model::{AccountView, LoginHealth, Provider, Window},
    usage,
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Utc};

fn account(email: &str, weekly: f64, known: bool, severity: &str, fetched_at: i64) -> AccountView {
    AccountView {
        provider: Provider::Claude,
        email: email.into(),
        fetched_at,
        login: LoginHealth {
            state: "ok".into(),
            ..Default::default()
        },
        windows: vec![
            Window {
                key: "five_hour".into(),
                group: "session".into(),
                label: "5h session".into(),
                known: true,
                ..Default::default()
            },
            Window {
                key: "weekly_all".into(),
                group: "weekly".into(),
                label: "7d all".into(),
                percent: weekly,
                known,
                severity: severity.into(),
                resets_at: "2026-01-08T00:00:00Z".into(),
            },
        ],
        ..Default::default()
    }
}

#[test]
fn known_usable_account_beats_unknown_locked_capacity() {
    let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
        .unwrap()
        .with_timezone(&Utc);
    let mut locked = account(
        "locked@example.test",
        1.0,
        true,
        "ok",
        now.timestamp_millis(),
    );
    locked.windows[0].known = false;
    locked.windows[0].severity = "locked".into();
    let mut unknown = account(
        "unknown@example.test",
        30.0,
        false,
        "",
        now.timestamp_millis(),
    );
    unknown.windows[0].known = false;
    let usable = account(
        "usable@example.test",
        30.0,
        true,
        "ok",
        now.timestamp_millis(),
    );
    let mut accounts = vec![locked, unknown, usable];
    usage::recommend(&mut accounts, now);
    assert!(!accounts[0].recommended);
    assert!(!accounts[1].recommended);
    assert!(accounts[2].recommended);
    assert!(accounts[2].why.contains("70% of the week left"));
}

#[test]
fn explicit_locks_override_known_windows_in_both_capacity_groups() {
    let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
        .unwrap()
        .with_timezone(&Utc);
    let mut locked = account(
        "locked@example.test",
        5.0,
        true,
        "ok",
        now.timestamp_millis(),
    );
    locked.windows.push(Window {
        key: "weekly_extra".into(),
        group: "weekly".into(),
        known: true,
        percent: 1.0,
        ..Default::default()
    });
    locked.windows.push(Window {
        key: "weekly_locked".into(),
        group: "weekly".into(),
        known: false,
        severity: "locked".into(),
        ..Default::default()
    });
    locked.windows.push(Window {
        key: "five_hour_locked".into(),
        group: "session".into(),
        known: false,
        severity: "locked".into(),
        ..Default::default()
    });
    let mut healthy = account(
        "healthy@example.test",
        30.0,
        true,
        "ok",
        now.timestamp_millis(),
    );
    healthy.windows[0].percent = 1.0;
    let mut accounts = vec![locked, healthy];
    usage::recommend(&mut accounts, now);
    assert!(!accounts[0].recommended);
    assert!(accounts[1].recommended);
}

#[test]
fn stale_usage_is_displayable_but_cannot_drive_recommendation() {
    let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
        .unwrap()
        .with_timezone(&Utc);
    let mut accounts = vec![account(
        "old@example.test",
        1.0,
        true,
        "ok",
        now.timestamp_millis() - 15 * 60 * 1000 - 1,
    )];
    usage::recommend(&mut accounts, now);
    assert!(!accounts[0].recommended);
}

#[test]
fn fresh_usage_beats_lower_percentage_marked_stale() {
    let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
        .unwrap()
        .with_timezone(&Utc);
    let mut stale = account(
        "stale@example.test",
        1.0,
        true,
        "ok",
        now.timestamp_millis() - 5 * 60 * 1000,
    );
    stale.stale = "usage request failed; showing cached data".into();
    let fresh = account(
        "fresh@example.test",
        30.0,
        true,
        "ok",
        now.timestamp_millis(),
    );
    let mut accounts = vec![stale, fresh];
    usage::recommend(&mut accounts, now);
    assert!(!accounts[0].recommended);
    assert!(accounts[1].recommended);
}

#[test]
fn codex_jwt_and_explicit_account_id_share_identity_key() {
    let payload = URL_SAFE_NO_PAD.encode(
        br#"{"email":"same@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}"#,
    );
    let jwt = format!("e30.{payload}.x");
    let explicit = live::codex_identity(&jwt, "acct-1");
    let from_jwt = live::codex_identity(&jwt, "");
    assert_eq!(explicit.account_id, "acct-1");
    assert_eq!(from_jwt.account_id, "acct-1");
    assert_eq!(explicit.org_uuid, from_jwt.org_uuid);
    assert_eq!(explicit.key(), from_jwt.key());
}
