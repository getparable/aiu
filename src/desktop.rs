use aiu::{
    app::App,
    config::Config,
    model::{Provider, Snapshot},
    provider::redact,
};
use anyhow::Result;
use eframe::egui::{self, Color32, RichText};
use std::{
    path::PathBuf,
    sync::mpsc::{self, Receiver},
    time::{Duration, Instant},
};

pub fn run(config: Config, fixture: Option<PathBuf>) -> Result<()> {
    let initial = fixture.as_deref().map(super::read_fixture).transpose()?;
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_title("AIU — account usage")
            .with_inner_size([620.0, 760.0])
            .with_min_inner_size([450.0, 360.0]),
        ..Default::default()
    };
    eframe::run_native(
        "AIU",
        options,
        Box::new(move |cc| {
            cc.egui_ctx.set_visuals(egui::Visuals::dark());
            let mut panel = Panel {
                config,
                fixture,
                snapshot: initial.unwrap_or_default(),
                pending: None,
                message: String::new(),
                last_poll: Instant::now(),
                provider: Provider::Claude,
                label: String::new(),
                filter: None,
            };
            if panel.fixture.is_none() {
                panel.start(Action::Refresh, cc.egui_ctx.clone());
            } else {
                panel.message = "Sample data — account actions are disabled".into();
            }
            Ok(Box::new(panel))
        }),
    )
    .map_err(|e| anyhow::anyhow!("desktop panel: {e}"))
}

enum Action {
    Refresh,
    Add(Provider, String),
    Login(Provider, String),
    Switch(String),
    Remove(String),
}
struct Panel {
    config: Config,
    fixture: Option<PathBuf>,
    snapshot: Snapshot,
    pending: Option<Receiver<Result<(Snapshot, String)>>>,
    message: String,
    last_poll: Instant,
    provider: Provider,
    label: String,
    filter: Option<Provider>,
}

impl Panel {
    fn start(&mut self, action: Action, context: egui::Context) {
        if self.pending.is_some() {
            return;
        }
        let config = self.config.clone();
        let fixture = self.fixture.clone();
        let (tx, rx) = mpsc::channel();
        self.message = match action {
            Action::Login(..) => "Complete sign-in in your browser. Waiting up to five minutes…",
            _ => "Updating…",
        }
        .into();
        self.pending = Some(rx);
        std::thread::spawn(move || {
            let result = (|| {
                if let Some(path) = fixture {
                    return Ok((
                        super::read_fixture(&path)?,
                        "Sample data — account actions are disabled".into(),
                    ));
                }
                let app = App::new(config)?;
                let mut message = String::new();
                match action {
                    Action::Refresh => {}
                    Action::Add(provider, label) => {
                        let saved = app.add(provider, Some(&label))?;
                        message = format!("Saved {}", saved.label);
                    }
                    Action::Login(provider, label) => {
                        let session = app.api.begin_login(provider, false, false, false)?;
                        app.api.open_browser(&session.authorize_url)?;
                        let code = session.wait_for_code()?;
                        let record = app.api.complete_login(&session, &code)?;
                        let saved = app.save_login(record, Some(&label))?;
                        message = format!("Signed in as {}", saved.email);
                    }
                    Action::Switch(key) => {
                        let warnings = app.switch(&key, None)?;
                        message = format!(
                            "Switched account. Start a new CLI session. {}",
                            warnings.join("; ")
                        );
                    }
                    Action::Remove(key) => {
                        let removed = app.remove(&key, None)?;
                        message = format!("Removed {} from AIU", removed.label);
                    }
                }
                let snapshot = app.status(None, false)?;
                Ok((snapshot, message))
            })();
            let _ = tx.send(result);
            context.request_repaint();
        });
    }
}

impl eframe::App for Panel {
    fn update(&mut self, ctx: &egui::Context, _: &mut eframe::Frame) {
        #[cfg(feature = "screenshot")]
        if self.fixture.is_some()
            && let Some(path) = std::env::var_os("AIU_SCREENSHOT_TO")
        {
            if ctx.cumulative_frame_nr() == 10 {
                ctx.send_viewport_cmd(egui::ViewportCommand::Screenshot(Default::default()));
            }
            let captured = ctx.input(|input| {
                input.events.iter().find_map(|event| match event {
                    egui::Event::Screenshot { image, .. } => Some(image.clone()),
                    _ => None,
                })
            });
            if let Some(captured) = captured {
                let pixels = captured.pixels.iter().flat_map(|p| p.to_array()).collect();
                if let Some(bitmap) = image::RgbaImage::from_raw(
                    captured.size[0] as u32,
                    captured.size[1] as u32,
                    pixels,
                ) && let Err(error) = bitmap.save(&path)
                {
                    eprintln!("screenshot failed: {error}");
                }
                ctx.send_viewport_cmd(egui::ViewportCommand::Close);
            }
            ctx.request_repaint_after(Duration::from_millis(20));
        }
        if let Some(rx) = &self.pending {
            match rx.try_recv() {
                Ok(result) => {
                    self.pending = None;
                    self.last_poll = Instant::now();
                    match result {
                        Ok((snapshot, message)) => {
                            self.snapshot = snapshot;
                            self.message = message;
                        }
                        Err(e) => self.message = redact(&format!("{e:#}")),
                    }
                }
                Err(mpsc::TryRecvError::Disconnected) => {
                    self.pending = None;
                    self.message = "Background task ended unexpectedly".into();
                    self.last_poll = Instant::now();
                }
                Err(mpsc::TryRecvError::Empty) => {}
            }
        }
        if self.pending.is_none() && self.last_poll.elapsed() >= Duration::from_secs(60) {
            self.start(Action::Refresh, ctx.clone());
        }
        ctx.request_repaint_after(Duration::from_secs(1));
        let busy = self.pending.is_some();
        let can_edit = !busy && self.fixture.is_none();
        let mut action = None;

        egui::TopBottomPanel::top("header").show(ctx, |ui| {
            ui.add_space(12.0);
            ui.horizontal(|ui| {
                ui.heading(
                    RichText::new("AIU")
                        .size(28.0)
                        .color(Color32::from_rgb(133, 203, 193)),
                );
                ui.label("Your account usage");
                ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                    if ui
                        .add_enabled(!busy, egui::Button::new("Refresh"))
                        .clicked()
                    {
                        action = Some(Action::Refresh);
                    }
                    if busy {
                        ui.spinner();
                    }
                });
            });
            ui.add_space(8.0);
            ui.horizontal(|ui| {
                ui.selectable_value(&mut self.filter, None, "All accounts");
                ui.selectable_value(&mut self.filter, Some(Provider::Claude), "Claude");
                ui.selectable_value(&mut self.filter, Some(Provider::Codex), "Codex");
            });
            ui.add_space(8.0);
        });

        egui::TopBottomPanel::bottom("footer").show(ctx, |ui| {
            ui.add_space(8.0);
            ui.horizontal(|ui| {
                egui::ComboBox::from_id_salt("add_provider").selected_text(self.provider.client()).show_ui(ui, |ui| {
                    ui.selectable_value(&mut self.provider, Provider::Claude, "Claude Code");
                    ui.selectable_value(&mut self.provider, Provider::Codex, "Codex");
                });
                ui.add(egui::TextEdit::singleline(&mut self.label).hint_text("Label (optional)").desired_width(120.0));
                if ui.add_enabled(can_edit, egui::Button::new("Add current")).clicked() { action = Some(Action::Add(self.provider, self.label.clone())); }
                if ui.add_enabled(can_edit, egui::Button::new("Sign in")).clicked() { action = Some(Action::Login(self.provider, self.label.clone())); }
            });
            if !self.message.is_empty() { ui.label(&self.message); }
            for warning in &self.snapshot.warnings { ui.colored_label(Color32::from_rgb(240, 184, 98), warning); }
            ui.small("Display refreshes every minute. Usage requests are shared and cached for five minutes.");
            ui.add_space(6.0);
        });

        egui::CentralPanel::default().show(ctx, |ui| {
            egui::ScrollArea::vertical().show(ui, |ui| {
                if self.snapshot.accounts.is_empty() {
                    ui.add_space(50.0);
                    ui.heading("Bring your accounts together");
                    ui.label("Add the account your CLI already uses, or sign in to another account below.");
                }
                for account in self.snapshot.accounts.iter().filter(|a| self.filter.is_none_or(|p| p == a.provider)) {
                    egui::Frame::group(ui.style()).inner_margin(14.0).show(ui, |ui| {
                        ui.set_min_width(ui.available_width());
                        ui.horizontal(|ui| {
                            ui.heading(&account.label);
                            ui.weak(format!("{} · {}", account.provider, account.tier));
                            if account.active { ui.colored_label(Color32::from_rgb(133, 203, 193), "Active"); }
                        });
                        ui.label(&account.email);
                        if !account.org_name.is_empty() { ui.weak(&account.org_name); }
                        ui.add_space(8.0);
                        if !account.error.is_empty() { ui.colored_label(Color32::from_rgb(241, 129, 129), &account.error); }
                        for window in &account.windows {
                            ui.horizontal(|ui| { ui.label(&window.label); ui.with_layout(egui::Layout::right_to_left(egui::Align::Center), |ui| {
                                ui.label(if window.known { format!("{:.0}% used", window.percent) } else { "Unknown".into() });
                            }); });
                            let color = if window.percent >= 80.0 { Color32::from_rgb(222, 124, 126) } else if window.percent >= 60.0 { Color32::from_rgb(227, 180, 102) } else { Color32::from_rgb(107, 179, 164) };
                            ui.add(egui::ProgressBar::new((window.percent / 100.0) as f32).fill(color).desired_height(7.0));
                            if !window.resets_at.is_empty() {
                                let display = chrono::DateTime::parse_from_rfc3339(&window.resets_at).map(|t| t.with_timezone(&chrono::Local).format("%a %b %d, %I:%M %p").to_string()).unwrap_or_else(|_| window.resets_at.clone());
                                ui.small(format!("Resets {display}"));
                            }
                            ui.add_space(5.0);
                        }
                        if account.recommended { ui.colored_label(Color32::from_rgb(133, 203, 193), format!("{} · {}", if account.all_spent { "Next reset" } else { "Use next" }, account.why)); }
                        if !account.stale.is_empty() { ui.small(format!("Cached: {}", account.stale)); }
                        ui.horizontal(|ui| {
                            let switchable = can_edit && !account.read_only && !["expired", "missing"].contains(&account.login.state.as_str());
                            if ui.add_enabled(switchable && !account.active, egui::Button::new("Switch")).clicked() { action = Some(Action::Switch(account.key.clone())); }
                            ui.weak(&account.login.message);
                            ui.menu_button("More", |ui| {
                                if ui.add_enabled(can_edit, egui::Button::new("Forget in AIU")).clicked() { action = Some(Action::Remove(account.key.clone())); ui.close(); }
                            });
                        });
                    });
                    ui.add_space(10.0);
                }
            });
        });
        if let Some(action) = action {
            self.start(action, ctx.clone());
        }
    }
}
