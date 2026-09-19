//! Provider independent usage windows and account selection.

use chrono::{DateTime, SecondsFormat, Utc};
use regex::Regex;
use serde_json::{Map, Value};
use std::cmp::Ordering;

const RECOMMENDATION_MAX_AGE_MS: i64 = 15 * 60 * 1000;

use crate::model::{AccountView, LoginHealth, Provider, Record, Window};

fn num(v: Option<&Value>) -> Option<f64> {
    match v? {
        Value::Number(n) => n.as_f64(),
        Value::String(s) => s.parse().ok(),
        _ => None,
    }
}
fn text(v: Option<&Value>) -> String {
    v.and_then(Value::as_str).unwrap_or("").to_string()
}
fn object(v: Option<&Value>) -> Option<&Map<String, Value>> {
    v.and_then(Value::as_object)
}
fn clamp(v: Option<&Value>) -> (f64, bool) {
    match num(v).filter(|n| n.is_finite()) {
        Some(n) => (n.clamp(0.0, 100.0), true),
        None => (0.0, false),
    }
}
fn timestamp(s: &str) -> String {
    if s.is_empty() {
        return String::new();
    }
    DateTime::parse_from_rfc3339(s)
        .map(|t| {
            t.with_timezone(&Utc)
                .to_rfc3339_opts(SecondsFormat::Secs, true)
        })
        .unwrap_or_else(|_| s.to_string())
}
fn severity(p: f64) -> String {
    if p >= 80.0 {
        "critical".into()
    } else if p >= 60.0 {
        "warning".into()
    } else {
        "ok".into()
    }
}

/// Convert Claude and Codex usage response shapes to the stable window model.
pub fn normalize_windows(raw: &Value, now: DateTime<Utc>) -> Vec<Window> {
    let Some(body) = raw.as_object() else {
        return vec![];
    };
    if let Some(limits) = body
        .get("limits")
        .and_then(Value::as_array)
        .filter(|v| !v.is_empty())
    {
        return limit_windows(limits);
    }
    if body.contains_key("rate_limit") || body.contains_key("plan_type") {
        return codex_windows(body, now);
    }
    let order = [
        "five_hour",
        "seven_day",
        "seven_day_opus",
        "seven_day_sonnet",
        "seven_day_oauth_apps",
    ];
    let mut keys: Vec<&String> = body
        .iter()
        .filter_map(|(k, v)| {
            let m = v.as_object()?;
            if ["extra_usage", "spend"].contains(&k.as_str()) {
                return None;
            }
            m.contains_key("utilization").then_some(k)
        })
        .collect();
    keys.sort_by_key(|k| {
        (
            order.iter().position(|x| *x == k.as_str()).unwrap_or(99),
            k.as_str(),
        )
    });
    keys.into_iter()
        .filter_map(|k| {
            let m = body.get(k)?.as_object()?;
            let (p, known) = clamp(m.get("utilization"));
            if order.iter().position(|x| *x == k.as_str()).is_none()
                && p <= 0.0
                && text(m.get("resets_at")).is_empty()
            {
                return None;
            }
            let label = match k.as_str() {
                "five_hour" => "5h session",
                "seven_day" => "7d all",
                "seven_day_opus" => "7d Opus",
                "seven_day_sonnet" => "7d Sonnet",
                "seven_day_oauth_apps" => "7d OAuth apps",
                _ => k.as_str(),
            };
            let sev = if !text(m.get("locked_reason")).is_empty() {
                "locked".into()
            } else if known {
                severity(p)
            } else {
                String::new()
            };
            Some(Window {
                key: k.clone(),
                group: if k == "five_hour" {
                    "session"
                } else {
                    "weekly"
                }
                .into(),
                label: label.into(),
                percent: p,
                known,
                resets_at: timestamp(&text(m.get("resets_at"))),
                severity: sev,
            })
        })
        .collect()
}

fn limit_windows(limits: &[Value]) -> Vec<Window> {
    limits
        .iter()
        .filter_map(|v| {
            let m = v.as_object()?;
            let kind = text(m.get("kind"));
            let scope = object(m.get("scope"));
            let model = scope
                .and_then(|s| object(s.get("model")))
                .map(|s| text(s.get("display_name")).if_empty_then(|| text(s.get("id"))))
                .unwrap_or_default();
            let scope_name = if model.is_empty() {
                scope.map(|s| text(s.get("surface"))).unwrap_or_default()
            } else {
                model
            };
            let mut group = text(m.get("group"));
            if group.is_empty() {
                group = if kind == "session" {
                    "session"
                } else {
                    "weekly"
                }
                .into();
            }
            let label = if kind == "session" {
                "5h session".into()
            } else if kind == "weekly_all" {
                "7d all".into()
            } else if group == "weekly" {
                format!(
                    "7d {}",
                    if scope_name.is_empty() {
                        kind.trim_start_matches("weekly_")
                    } else {
                        &scope_name
                    }
                )
            } else if !scope_name.is_empty() {
                format!("{} {}", kind, scope_name)
            } else {
                kind.clone()
            };
            let key = if scope_name.is_empty() {
                kind
            } else {
                format!("{}:{}", kind, scope_name)
            };
            let (p, known) = clamp(m.get("percent"));
            let mut sev = text(m.get("severity"));
            if sev == "normal" {
                sev.clear();
            }
            Some(Window {
                key,
                group,
                label,
                percent: p,
                known,
                resets_at: timestamp(&text(m.get("resets_at"))),
                severity: sev,
            })
        })
        .collect()
}

fn codex_windows(body: &Map<String, Value>, now: DateTime<Utc>) -> Vec<Window> {
    let Some(main) = object(body.get("rate_limit")) else {
        return vec![];
    };
    ["primary_window", "secondary_window"]
        .iter()
        .filter_map(|key| {
            let m = object(main.get(*key))?;
            let secs = num(m.get("limit_window_seconds")).unwrap_or(0.0);
            let hours = (secs / 3600.0).round() as i64;
            let span = if hours >= 24 {
                format!("{}d", (hours as f64 / 24.0).round() as i64)
            } else if hours > 0 {
                format!("{}h", hours)
            } else {
                "?h".into()
            };
            let session = secs > 0.0 && secs <= 86400.0;
            let (p, known) = clamp(m.get("used_percent"));
            let reset = if let Some(n) = num(m.get("reset_at")).filter(|n| *n > 0.0) {
                DateTime::<Utc>::from_timestamp(n as i64, 0)
                    .map(|x| x.to_rfc3339_opts(SecondsFormat::Secs, true))
                    .unwrap_or_default()
            } else if let Some(n) = num(m.get("reset_after_seconds")) {
                chrono::Duration::try_seconds(n as i64)
                    .and_then(|d| now.checked_add_signed(d))
                    .map(|t| t.to_rfc3339_opts(SecondsFormat::Secs, true))
                    .unwrap_or_default()
            } else {
                String::new()
            };
            let sev = if main.get("allowed").and_then(Value::as_bool) == Some(false) && p >= 100.0 {
                "locked"
            } else if known {
                &severity(p)
            } else {
                ""
            };
            Some(Window {
                key: key.trim_end_matches("_window").into(),
                group: if session { "session" } else { "weekly" }.into(),
                label: format!("{} {}", span, if session { "session" } else { "all" }),
                percent: p,
                known,
                resets_at: reset,
                severity: sev.into(),
            })
        })
        .collect()
}

trait IfEmpty {
    fn if_empty_then<F: FnOnce() -> String>(self, f: F) -> String;
}
impl IfEmpty for String {
    fn if_empty_then<F: FnOnce() -> String>(self, f: F) -> String {
        if self.is_empty() { f() } else { self }
    }
}

pub fn tier_label(r: &Record) -> String {
    if r.provider == Provider::Codex {
        let p = r.plan_type.to_lowercase();
        let n = match p.as_str() {
            "free" => "Free",
            "plus" => "Plus",
            "pro" => "Pro",
            "team" => "Team",
            "business" => "Business",
            "enterprise" => "Enterprise",
            "edu" => "Edu",
            _ => "",
        };
        return if !n.is_empty() {
            format!("ChatGPT {n}")
        } else if !p.is_empty() {
            let mut chars = p.chars();
            let first = chars.next().unwrap().to_uppercase().collect::<String>();
            format!("ChatGPT {}{}", first, chars.collect::<String>())
        } else {
            "ChatGPT".into()
        };
    }
    let tier = if !r.rate_limit_tier.is_empty() {
        r.rate_limit_tier.clone()
    } else {
        r.profile
            .get("organizationRateLimitTier")
            .and_then(Value::as_str)
            .unwrap_or("")
            .into()
    };
    let re = Regex::new(r"(?i)(pro|max)_?(\d+x)?").unwrap();
    if let Some(c) = re.captures(&tier) {
        return format!(
            "{}{}",
            if c[1].eq_ignore_ascii_case("pro") {
                "Pro"
            } else {
                "Max"
            },
            c.get(2)
                .map(|x| format!(" {}", x.as_str()))
                .unwrap_or_default()
        );
    }
    r.subscription_type.clone()
}

pub fn login_health(r: &Record, needs_login: bool, now: i64) -> LoginHealth {
    if r.missing {
        return LoginHealth {
            state: "missing".into(),
            message: "no stored token — sign in again".into(),
        };
    }
    if needs_login {
        return LoginHealth {
            state: "expired".into(),
            message: "login expired or revoked — sign in again".into(),
        };
    }
    if r.refresh_token_expires_at > 0 {
        let left = r.refresh_token_expires_at - now;
        if left <= 0 {
            return LoginHealth {
                state: "expired".into(),
                message: "login expired — sign in again".into(),
            };
        }
        if left < 5 * 24 * 60 * 60 * 1000 {
            return LoginHealth {
                state: "expiring".into(),
                message: format!("login expires in {}", relative(left)),
            };
        }
        return LoginHealth {
            state: "ok".into(),
            message: format!("login valid for {}", relative(left)),
        };
    }
    if r.provider == Provider::Codex {
        return LoginHealth {
            state: "ok".into(),
            message: "login active".into(),
        };
    }
    LoginHealth {
        state: "unknown".into(),
        message: "login lifetime unknown".into(),
    }
}
fn relative(ms: i64) -> String {
    let h = ms / 3_600_000;
    if h >= 24 {
        format!("{}d {}h", h / 24, h % 24)
    } else {
        format!("{}h", h)
    }
}

fn scoped(w: &Window) -> bool {
    w.group == "weekly"
        && !["seven_day", "weekly_all", "primary", "secondary"].contains(&w.key.as_str())
}
#[derive(Clone)]
struct Fit {
    weekly: f64,
    session: Option<f64>,
    stale: bool,
    score: f64,
    flagship: Option<f64>,
    tier: String,
}

fn fit(a: &AccountView, now: DateTime<Utc>) -> Option<Fit> {
    if !a.error.is_empty()
        || a.read_only
        || ["missing", "expired"].contains(&a.login.state.as_str())
        || a.fetched_at <= 0
        || now.timestamp_millis().saturating_sub(a.fetched_at) > RECOMMENDATION_MAX_AGE_MS
    {
        return None;
    }
    let weekly_windows: Vec<&Window> = a
        .windows
        .iter()
        .filter(|w| w.group == "weekly" && !scoped(w))
        .collect();
    let weekly_locked = weekly_windows.iter().any(|w| w.severity == "locked");
    let weekly_unknown = weekly_windows
        .iter()
        .any(|w| !w.known && w.severity != "locked");
    if weekly_unknown {
        return None;
    }
    let weekly = if weekly_locked {
        100.0
    } else {
        weekly_windows
            .iter()
            .filter(|w| w.known)
            .map(|w| w.percent)
            .max_by(|x, y| x.partial_cmp(y).unwrap_or(Ordering::Equal))?
    };
    let session_windows: Vec<&Window> = a.windows.iter().filter(|w| w.group == "session").collect();
    let session_locked = session_windows.iter().any(|w| w.severity == "locked");
    let session_unknown = session_windows
        .iter()
        .any(|w| !w.known && w.severity != "locked");
    if session_unknown {
        return None;
    }
    let session = if session_windows.is_empty() {
        Some(0.0)
    } else if session_locked {
        Some(100.0)
    } else {
        session_windows
            .iter()
            .filter(|w| w.known)
            .map(|w| w.percent)
            .max_by(|x, y| x.partial_cmp(y).unwrap_or(Ordering::Equal))
            .or_else(|| {
                session_windows
                    .iter()
                    .any(|w| w.severity == "locked")
                    .then_some(100.0)
            })
    };
    let flag = a
        .windows
        .iter()
        .filter(|w| {
            scoped(w)
                && (w.known || w.severity == "locked")
                && ["fable", "opus"]
                    .iter()
                    .any(|n| w.label.to_lowercase().contains(n) || w.key.to_lowercase().contains(n))
        })
        .map(|w| {
            if w.severity == "locked" {
                100.0
            } else {
                w.percent
            }
        })
        .max_by(|x, y| x.partial_cmp(y).unwrap_or(Ordering::Equal));
    let weight = Regex::new(r"(?i)(\d+)\s*x\b")
        .unwrap()
        .captures(&a.tier)
        .and_then(|c| c[1].parse::<f64>().ok())
        .unwrap_or(1.0);
    Some(Fit {
        weekly,
        session,
        stale: !a.stale.is_empty(),
        score: (100.0 - weekly) * weight,
        flagship: flag,
        tier: a.tier.clone(),
    })
}

/// Mark one recommendation per provider. Invalid/unknown accounts are left unmarked.
pub fn recommend(accounts: &mut [AccountView], now: DateTime<Utc>) {
    for a in accounts.iter_mut() {
        a.recommended = false;
        a.all_spent = false;
        a.why.clear();
    }
    for provider in [Provider::Claude, Provider::Codex] {
        let ids: Vec<usize> = accounts
            .iter()
            .enumerate()
            .filter(|(_, a)| a.provider == provider)
            .map(|(i, _)| i)
            .collect();
        let flagship_matters = ids
            .iter()
            .filter_map(|&i| fit(&accounts[i], now).and_then(|f| f.flagship))
            .next()
            .is_some();
        let mut valid: Vec<(usize, Fit)> = ids
            .iter()
            .filter_map(|&i| fit(&accounts[i], now).map(|f| (i, f)))
            .collect();
        if valid.is_empty() {
            continue;
        }
        let usable = |f: &Fit| {
            f.weekly < 100.0
                && f.session.is_some_and(|session| session < 100.0)
                && (!flagship_matters || f.flagship.map(|x| x < 100.0).unwrap_or(false))
        };
        valid.sort_by(|a, b| {
            let (af, bf) = (&a.1, &b.1);
            let band = |f: &Fit| {
                (
                    f.weekly >= 100.0,
                    f.session.map(|session| session >= 100.0).unwrap_or(true),
                    flagship_matters && f.flagship.map(|x| x >= 100.0).unwrap_or(true),
                    f.stale,
                )
            };
            band(af)
                .cmp(&band(bf))
                .then_with(|| bf.score.partial_cmp(&af.score).unwrap_or(Ordering::Equal))
                .then_with(|| {
                    af.session
                        .partial_cmp(&bf.session)
                        .unwrap_or(Ordering::Equal)
                })
        });
        let all_spent = valid.iter().all(|(_, f)| f.weekly >= 100.0);
        let pick = if all_spent {
            valid
                .iter()
                .filter_map(|(i, _)| {
                    let at = accounts[*i]
                        .windows
                        .iter()
                        .find(|w| w.group == "weekly" && !scoped(w))
                        .and_then(|w| DateTime::parse_from_rfc3339(&w.resets_at).ok())
                        .map(|x| x.with_timezone(&Utc))
                        .filter(|at| *at > now);
                    Some((*i, at?))
                })
                .min_by_key(|(_, at)| *at)
                .map(|x| x.0)
                .or_else(|| valid.first().map(|x| x.0))
        } else {
            valid
                .iter()
                .find(|(_, f)| usable(f))
                .map(|x| x.0)
                .or_else(|| valid.first().map(|x| x.0))
        };
        if let Some(i) = pick {
            let f = fit(&accounts[i], now).unwrap();
            accounts[i].recommended = true;
            accounts[i].all_spent = all_spent;
            let mut why = if all_spent {
                "all weekly room spent".to_string()
            } else {
                format!("{:.0}% of the week left", 100.0 - f.weekly)
            };
            if !f.tier.is_empty() {
                why.push_str(&format!(" · {}", f.tier));
            }
            if f.session.is_some_and(|session| session >= 100.0) {
                why.push_str(" · 5h limit hit");
            }
            if flagship_matters && f.flagship.map(|x| x >= 100.0).unwrap_or(true) && !all_spent {
                why.push_str(" · no Fable");
            }
            accounts[i].why = why;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn normalizes_both_provider_shapes_and_clamps() {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .unwrap()
            .with_timezone(&Utc);
        let claude = normalize_windows(
            &json!({
                "five_hour": {"utilization": 120, "resets_at":"2026-01-01T01:00:00-05:00"},
                "seven_day": {"utilization": -2}, "extra_usage": {"utilization": 99}
            }),
            now,
        );
        assert_eq!(claude.len(), 2);
        assert_eq!(claude[0].percent, 100.0);
        assert_eq!(claude[0].group, "session");
        assert_eq!(claude[0].resets_at, "2026-01-01T06:00:00Z");
        let codex = normalize_windows(
            &json!({"plan_type":"pro", "rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":40,"limit_window_seconds":604800}}}),
            now,
        );
        assert_eq!(codex[0].label, "5h session");
        assert_eq!(codex[1].label, "7d all");
        assert!(codex[0].resets_at.ends_with('Z'));
    }

    #[test]
    fn login_and_tier_labels_match_legacy_contract() {
        let mut r = Record {
            provider: Provider::Claude,
            rate_limit_tier: "default_claude_max_20x".into(),
            ..Default::default()
        };
        assert_eq!(tier_label(&r), "Max 20x");
        r = Record {
            provider: Provider::Codex,
            plan_type: "pro".into(),
            ..Default::default()
        };
        assert_eq!(tier_label(&r), "ChatGPT Pro");
        r = Record {
            provider: Provider::Claude,
            refresh_token_expires_at: 1_000_000,
            ..Default::default()
        };
        assert_eq!(login_health(&r, false, 1_000_000).state, "expired");
    }

    fn account(
        provider: Provider,
        tier: &str,
        session: f64,
        weekly: f64,
        flagship: Option<f64>,
    ) -> AccountView {
        let mut windows = vec![
            Window {
                key: "five_hour".into(),
                group: "session".into(),
                label: "5h session".into(),
                percent: session,
                known: true,
                ..Default::default()
            },
            Window {
                key: "weekly_all".into(),
                group: "weekly".into(),
                label: "7d all".into(),
                percent: weekly,
                known: true,
                ..Default::default()
            },
        ];
        if let Some(p) = flagship {
            windows.push(Window {
                key: "weekly_scoped:Fable".into(),
                group: "weekly".into(),
                label: "7d Fable".into(),
                percent: p,
                known: true,
                ..Default::default()
            });
        }
        AccountView {
            provider,
            tier: tier.into(),
            windows,
            fetched_at: 1_767_225_600_000,
            login: LoginHealth {
                state: "ok".into(),
                ..Default::default()
            },
            ..Default::default()
        }
    }

    #[test]
    fn recommendation_applies_gates_capacity_and_provider_isolation() {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .unwrap()
            .with_timezone(&Utc);
        let mut a = account(Provider::Claude, "Max 20x", 0.0, 50.0, Some(50.0));
        let b = account(Provider::Claude, "Pro", 0.0, 2.0, None);
        let c = account(Provider::Codex, "ChatGPT Pro", 0.0, 80.0, None);
        recommend(std::slice::from_mut(&mut a), now);
        assert!(a.recommended);
        let mut all = vec![a, b, c];
        recommend(&mut all, now);
        assert_eq!(
            all.iter()
                .filter(|x| x.provider == Provider::Claude && x.recommended)
                .count(),
            1
        );
        assert_eq!(
            all.iter()
                .filter(|x| x.provider == Provider::Codex && x.recommended)
                .count(),
            1
        );
        assert!(
            all[0].recommended,
            "known flagship access and 20x capacity beat the unscoped Pro account"
        );
        assert!(!all[1].recommended);
        assert!(all[0].why.contains("50% of the week left"));
        assert!(all[0].why.contains("Max 20x"));
    }

    #[test]
    fn recommendation_gates_session_before_plan_capacity() {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .unwrap()
            .with_timezone(&Utc);
        let blocked = account(Provider::Claude, "Max 20x", 100.0, 20.0, Some(20.0));
        let no_flagship = account(Provider::Claude, "Pro", 0.0, 20.0, None);
        let mut accounts = vec![blocked, no_flagship];
        recommend(&mut accounts, now);
        assert!(
            accounts[1].recommended,
            "session room outranks plan size and flagship gate"
        );
    }

    #[test]
    fn all_spent_uses_earliest_future_reset_and_ignores_expired_reset() {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .unwrap()
            .with_timezone(&Utc);
        let mut late = account(Provider::Codex, "Pro", 0.0, 100.0, None);
        late.windows[1].resets_at = "2026-01-04T00:00:00Z".into();
        let mut soon = account(Provider::Codex, "Pro", 0.0, 100.0, None);
        soon.windows[1].resets_at = "2025-12-31T00:00:00Z".into();
        let mut accounts = vec![late, soon];
        recommend(&mut accounts, now);
        assert!(accounts[0].recommended && accounts[0].all_spent);
        assert!(accounts[0].why.starts_with("all weekly room spent"));
    }
}
