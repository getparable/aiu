use aiu::{
    app::App,
    config::Config,
    model::{AccountView, Provider, Snapshot},
    provider::redact,
};
use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand, ValueEnum};
use std::{
    io::{self, IsTerminal, Write},
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

#[cfg(feature = "desktop")]
mod desktop;

#[derive(Parser)]
#[command(
    name = "aiu",
    version,
    about = "Claude and Codex usage across your accounts",
    long_about = "A native Rust account monitor for Windows, Linux and macOS. Run without a command to show usage. Credentials stay on your machine."
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
    #[arg(long, global = true)]
    json: bool,
    #[arg(long, global = true, value_enum, conflicts_with_all = ["claude", "codex"])]
    provider: Option<Provider>,
    #[arg(long, global = true, conflicts_with = "claude")]
    codex: bool,
    #[arg(long, global = true)]
    claude: bool,
    #[arg(long, global = true)]
    no_sync: bool,
    #[arg(long, global = true)]
    no_color: bool,
    #[arg(long, global = true, value_enum)]
    sort: Option<Sort>,
    /// Render saved JSON without reading credentials or contacting providers.
    #[arg(long, global = true, env = "AIU_JSON_FIXTURE")]
    fixture: Option<PathBuf>,
}

#[derive(Clone, Copy, ValueEnum)]
enum Sort {
    #[value(name = "5h")]
    Session,
    #[value(name = "7d")]
    Weekly,
}

#[derive(Subcommand)]
enum Command {
    /// Show usage (the default command).
    Status,
    /// Refresh the display; network requests remain limited to once per five minutes.
    Watch {
        #[arg(long, default_value_t = 60, value_parser = clap::value_parser!(u64).range(15..))]
        interval: u64,
    },
    /// Save the login currently used by Claude Code or Codex.
    Add {
        #[arg(long)]
        label: Option<String>,
    },
    /// Sign in through a browser and save an independent login.
    Login {
        #[arg(long)]
        label: Option<String>,
        #[arg(long)]
        readonly: bool,
        #[arg(long)]
        manual: bool,
        #[arg(long)]
        console: bool,
        #[arg(long)]
        no_open: bool,
    },
    /// List accounts without fetching usage.
    #[command(alias = "ls")]
    List,
    /// Forget an account in AIU; leaves the CLI's active login unchanged.
    #[command(alias = "rm")]
    Remove { target: String },
    /// Point Claude Code or Codex at a saved account.
    #[command(alias = "use")]
    Switch { target: String },
    /// Adopt newer tokens from the active CLI login.
    Sync,
    /// Identify the active CLI logins.
    Whoami,
    /// Open the portable desktop panel.
    #[command(alias = "menubar")]
    Gui,
    /// Check for a release; never installs or replaces files.
    Update {
        #[arg(long)]
        force: bool,
    },
    /// Show storage and CLI credential paths without reading tokens.
    Paths,
}

fn main() {
    if let Err(error) = run(Cli::parse()) {
        eprintln!("error: {}", redact(&format!("{error:#}")));
        std::process::exit(1);
    }
}

fn run(cli: Cli) -> Result<()> {
    let provider = cli.provider.or(if cli.codex {
        Some(Provider::Codex)
    } else if cli.claude {
        Some(Provider::Claude)
    } else {
        None
    });
    let command = cli.command.as_ref().unwrap_or(&Command::Status);
    if cli.fixture.is_some()
        && !matches!(
            command,
            Command::Status | Command::Watch { .. } | Command::Gui
        )
    {
        bail!("--fixture supports status, watch and gui only");
    }
    let config = Config::from_env()?;
    if matches!(command, Command::Gui) {
        #[cfg(feature = "desktop")]
        return desktop::run(config, cli.fixture);
        #[cfg(not(feature = "desktop"))]
        bail!("this build has no desktop panel; install without --no-default-features");
    }
    let app = App::new(config.clone())?;
    match command {
        Command::Status => output_snapshot(gather(&app, &cli, provider)?, cli.json),
        Command::Watch { interval } => {
            let running = Arc::new(AtomicBool::new(true));
            let signal = running.clone();
            ctrlc::set_handler(move || signal.store(false, Ordering::SeqCst))?;
            while running.load(Ordering::SeqCst) {
                if io::stdout().is_terminal() && !cli.json {
                    print!("\x1b[2J\x1b[H");
                }
                match gather(&app, &cli, provider) {
                    Ok(snapshot) => output_snapshot(snapshot, cli.json)?,
                    Err(e) => eprintln!("error: {}", redact(&e.to_string())),
                }
                io::stdout().flush()?;
                for _ in 0..interval.saturating_mul(4) {
                    if !running.load(Ordering::SeqCst) {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(250));
                }
            }
            Ok(())
        }
        Command::Add { label } => {
            let entry = app.add(provider.unwrap_or_default(), label.as_deref())?;
            if cli.json {
                println!("{}", serde_json::to_string_pretty(&entry)?);
            } else {
                println!(
                    "Saved {} as {}\nTokens: {}",
                    entry.key(),
                    entry.label,
                    config.storage_description()
                );
            }
            Ok(())
        }
        Command::Login {
            label,
            readonly,
            manual,
            console,
            no_open,
        } => {
            let session =
                app.api
                    .begin_login(provider.unwrap_or_default(), *readonly, *manual, *console)?;
            eprintln!(
                "Open this URL in a browser signed in to the account you want to add:\n{}",
                session.authorize_url
            );
            if !no_open && let Err(e) = app.api.open_browser(&session.authorize_url) {
                eprintln!("Could not open browser: {e}");
            }
            let code = if session.manual {
                eprint!("Paste the authorization code: ");
                io::stderr().flush()?;
                let mut code = String::new();
                io::stdin().read_line(&mut code)?;
                code
            } else {
                session.wait_for_code()?
            };
            let record = app.api.complete_login(&session, &code)?;
            let entry = app.save_login(record, label.as_deref())?;
            if cli.json {
                println!("{}", serde_json::to_string_pretty(&entry)?);
            } else {
                println!("Saved {} as {}", entry.key(), entry.label);
            }
            Ok(())
        }
        Command::List => {
            let accounts = app.list(provider)?;
            if cli.json {
                println!("{}", serde_json::to_string_pretty(&accounts)?);
            } else if accounts.is_empty() {
                println!("No accounts tracked. Run aiu add or aiu login.");
            } else {
                for a in accounts {
                    println!(
                        "{}  {}  {}{}",
                        a.key,
                        a.label,
                        a.tier,
                        if a.active { "  [active]" } else { "" }
                    );
                    println!("  {}", a.login.message);
                }
            }
            Ok(())
        }
        Command::Remove { target } => {
            let entry = app.remove(target, provider)?;
            if cli.json {
                println!("{}", serde_json::json!({"removed":entry.key()}));
            } else {
                println!("Removed {}", entry.key());
            }
            Ok(())
        }
        Command::Switch { target } => {
            let warnings = app.switch(target, provider)?;
            for warning in warnings {
                eprintln!("warning: {warning}");
            }
            if cli.json {
                println!("{}", serde_json::json!({"switched":target}));
            } else {
                println!(
                    "Switched to {target}. Start a new CLI session to use the selected account."
                );
            }
            Ok(())
        }
        Command::Sync => {
            let warnings = app.sync(provider)?;
            if cli.json {
                println!("{}", serde_json::json!({"synced":true,"warnings":warnings}));
            } else {
                println!("Synced newer tokens for tracked accounts.");
                for warning in warnings {
                    eprintln!("warning: {warning}");
                }
            }
            Ok(())
        }
        Command::Whoami => {
            let accounts = app.whoami(provider)?;
            if cli.json {
                println!("{}", serde_json::to_string_pretty(&accounts)?);
            } else {
                for a in accounts {
                    println!(
                        "{}: {}{}",
                        a.provider,
                        if a.email.is_empty() {
                            "not signed in or unverified"
                        } else {
                            &a.email
                        },
                        if a.org.is_empty() {
                            String::new()
                        } else {
                            format!(" #{}", a.org)
                        }
                    );
                }
            }
            Ok(())
        }
        Command::Paths => {
            let paths = serde_json::json!({"config":config.dir,"tokens":config.storage_description(),"claude":config.claude_dir,"codex":config.codex_home});
            println!("{}", serde_json::to_string_pretty(&paths)?);
            Ok(())
        }
        Command::Update { force } => check_update(&config, *force, cli.json),
        Command::Gui => unreachable!(),
    }
}

fn gather(app: &App, cli: &Cli, provider: Option<Provider>) -> Result<Snapshot> {
    let mut snapshot = if let Some(path) = &cli.fixture {
        read_fixture(path)?
    } else {
        app.status(provider, cli.no_sync)?
    };
    snapshot
        .accounts
        .retain(|a| provider.is_none_or(|p| a.provider == p));
    if let Some(sort) = cli.sort {
        let group = match sort {
            Sort::Session => "session",
            Sort::Weekly => "weekly",
        };
        snapshot
            .accounts
            .sort_by(|a, b| used(a, group).total_cmp(&used(b, group)));
    }
    Ok(snapshot)
}

pub(crate) fn read_fixture(path: &std::path::Path) -> Result<Snapshot> {
    let value: serde_json::Value = serde_json::from_slice(
        &std::fs::read(path).with_context(|| format!("reading fixture {}", path.display()))?,
    )?;
    if value.is_array() {
        let accounts: Vec<AccountView> = serde_json::from_value(value)?;
        Ok(Snapshot {
            empty: accounts.is_empty(),
            accounts,
            ..Default::default()
        })
    } else {
        Ok(serde_json::from_value(value)?)
    }
}

fn used(account: &AccountView, group: &str) -> f64 {
    if !account.error.is_empty() {
        return f64::INFINITY;
    }
    account
        .windows
        .iter()
        .filter(|w| w.group == group && w.known)
        .map(|w| w.percent)
        .reduce(f64::max)
        .unwrap_or(f64::INFINITY)
}

fn output_snapshot(snapshot: Snapshot, json: bool) -> Result<()> {
    for warning in &snapshot.warnings {
        eprintln!("warning: {warning}");
    }
    if json {
        println!("{}", serde_json::to_string_pretty(&snapshot.accounts)?);
        return Ok(());
    }
    if snapshot.accounts.is_empty() {
        println!("No accounts tracked for this view.\nRun aiu add, aiu add --codex, or aiu login.");
        return Ok(());
    }
    for a in &snapshot.accounts {
        println!(
            "{}  {}  {}{}",
            a.provider,
            a.label,
            a.tier,
            if a.active { "  [active]" } else { "" }
        );
        println!(
            "  {}{}",
            a.email,
            if a.org_name.is_empty() {
                String::new()
            } else {
                format!(" / {}", a.org_name)
            }
        );
        if !a.error.is_empty() {
            println!("  Error: {}", a.error);
        }
        for w in &a.windows {
            let filled = (w.percent / 5.0).round().clamp(0.0, 20.0) as usize;
            let bar = format!("{}{}", "#".repeat(filled), "-".repeat(20 - filled));
            println!(
                "  {:16} [{}] {:>6} used{}",
                w.label,
                bar,
                if w.known {
                    format!("{:.0}%", w.percent)
                } else {
                    "?".into()
                },
                if w.resets_at.is_empty() {
                    String::new()
                } else {
                    format!("  resets {}", w.resets_at)
                }
            );
        }
        if !a.stale.is_empty() {
            println!("  Cached: {}", a.stale);
        }
        if a.recommended {
            println!(
                "  {}: {}",
                if a.all_spent {
                    "Next reset"
                } else {
                    "Use next"
                },
                a.why
            );
        }
        println!();
    }
    Ok(())
}

#[derive(serde::Serialize, serde::Deserialize)]
struct ReleaseCache {
    checked_at: i64,
    tag: Option<String>,
    url: String,
}
fn check_update(config: &Config, force: bool, json: bool) -> Result<()> {
    let path = config.dir.join("release-cache.json");
    let _guard = aiu::storage::lock(&config.dir.join("release-cache.lock"))?;
    let now = chrono::Utc::now().timestamp();
    let cached: Option<ReleaseCache> = aiu::storage::read_json(&path)?;
    let release = if let Some(cached) =
        cached.filter(|c| !force && (0..21600).contains(&(now - c.checked_at)))
    {
        cached
    } else {
        let client = reqwest::blocking::Client::builder()
            .timeout(Duration::from_secs(20))
            .redirect(reqwest::redirect::Policy::none())
            .user_agent(concat!("aiu-rs/", env!("CARGO_PKG_VERSION")))
            .build()?;
        let response = client.get(&config.release_api_url).send()?;
        let release = if response.status() == reqwest::StatusCode::NOT_FOUND {
            ReleaseCache {
                checked_at: now,
                tag: None,
                url: String::new(),
            }
        } else {
            let value: serde_json::Value = response.error_for_status()?.json()?;
            ReleaseCache {
                checked_at: now,
                tag: Some(
                    value["tag_name"]
                        .as_str()
                        .context("release has no tag")?
                        .to_owned(),
                ),
                url: value["html_url"]
                    .as_str()
                    .unwrap_or("https://github.com/krflol/aiu-rs/releases")
                    .to_owned(),
            }
        };
        aiu::storage::write_json(&path, &release)?;
        release
    };
    let current = semver::Version::parse(env!("CARGO_PKG_VERSION"))?;
    let latest = release
        .tag
        .as_deref()
        .map(|s| semver::Version::parse(s.trim_start_matches('v')))
        .transpose()?;
    let available = latest.as_ref().is_some_and(|v| v > &current);
    if json {
        println!(
            "{}",
            serde_json::json!({"current":current.to_string(),"latest":release.tag,"available":available,"url":release.url})
        );
    } else if available {
        println!(
            "New release: {}\n{}\nInstall: cargo install --git https://github.com/krflol/aiu-rs --locked --force",
            release.tag.unwrap_or_default(),
            release.url
        );
    } else if latest.is_none() {
        println!("No Rust releases published yet. Installed version: {current}");
    } else {
        println!("aiu {current} is up to date.");
    }
    Ok(())
}
