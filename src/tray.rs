//! Native tray ownership and event delivery. The UI stays on eframe's event loop;
//! Linux's AppIndicator backend runs its required GTK loop on a dedicated thread.
use crate::desktop_wake::{Heartbeat, Wake};
use anyhow::Result;
use eframe::egui;
use std::sync::mpsc::{self, Receiver};
use tray_icon::{
    Icon, MouseButton, MouseButtonState, TrayIcon, TrayIconBuilder, TrayIconEvent,
    menu::{Menu, MenuEvent, MenuItem, PredefinedMenuItem},
};

#[derive(Clone, Copy)]
pub enum Command {
    Show,
    Refresh,
    Quit,
}

pub struct Tray {
    commands: Receiver<Command>,
    wake: Wake,
    _heartbeat: Heartbeat,
    #[cfg(not(target_os = "linux"))]
    _icon: TrayIcon,
    #[cfg(target_os = "linux")]
    stop: mpsc::Sender<()>,
    #[cfg(target_os = "linux")]
    thread: Option<std::thread::JoinHandle<()>>,
}

impl Tray {
    // Call once, from the first UI update: macOS requires a running main event loop.
    pub fn new(context: egui::Context, frame: &eframe::Frame) -> Result<Self> {
        let wake = Wake::new(context, frame)?;
        let heartbeat = Heartbeat::new(wake.clone());
        let (sender, commands) = mpsc::channel();
        let menu_sender = sender.clone();
        let menu_wake = wake.clone();
        MenuEvent::set_event_handler(Some(move |event: MenuEvent| {
            let command = match event.id.0.as_str() {
                "aiu-show" => Command::Show,
                "aiu-refresh" => Command::Refresh,
                "aiu-quit" => Command::Quit,
                _ => return,
            };
            let _ = menu_sender.send(command);
            menu_wake.request_repaint();
        }));
        let click_wake = wake.clone();
        TrayIconEvent::set_event_handler(Some(move |event| {
            if matches!(
                event,
                TrayIconEvent::Click {
                    button: MouseButton::Left,
                    button_state: MouseButtonState::Up,
                    ..
                }
            ) && !cfg!(target_os = "macos")
            {
                let _ = sender.send(Command::Show);
                click_wake.request_repaint();
            }
        }));

        #[cfg(not(target_os = "linux"))]
        {
            Ok(Self {
                commands,
                wake,
                _heartbeat: heartbeat,
                _icon: build_icon()?,
            })
        }
        #[cfg(target_os = "linux")]
        {
            let (stop, stopped) = mpsc::channel();
            let (ready, started) = mpsc::sync_channel(1);
            let thread = std::thread::spawn(move || {
                let icon = (|| {
                    gtk::init().map_err(|error| anyhow::anyhow!("GTK: {error}"))?;
                    ensure_status_notifier_host()?;
                    build_icon()
                })();
                let _icon = match icon {
                    Ok(icon) => {
                        if ready.send(Ok(())).is_err() {
                            return;
                        }
                        icon
                    }
                    Err(error) => {
                        let _ = ready.send(Err(error));
                        return;
                    }
                };
                loop {
                    while gtk::events_pending() {
                        gtk::main_iteration_do(false);
                    }
                    match stopped.recv_timeout(std::time::Duration::from_millis(100)) {
                        Err(mpsc::RecvTimeoutError::Timeout) => {}
                        _ => break,
                    }
                }
            });
            started.recv_timeout(std::time::Duration::from_secs(5))??;
            Ok(Self {
                commands,
                wake,
                _heartbeat: heartbeat,
                stop,
                thread: Some(thread),
            })
        }
    }

    pub fn commands(&self) -> impl Iterator<Item = Command> + '_ {
        self.commands.try_iter()
    }

    pub fn wake(&self) -> Wake {
        self.wake.clone()
    }
}

#[cfg(target_os = "linux")]
impl Drop for Tray {
    fn drop(&mut self) {
        let _ = self.stop.send(());
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

fn build_icon() -> Result<TrayIcon> {
    let menu = Menu::new();
    let show = MenuItem::with_id("aiu-show", "Show AIU", true, None);
    let refresh = MenuItem::with_id("aiu-refresh", "Refresh usage", true, None);
    let quit = MenuItem::with_id("aiu-quit", "Quit AIU", true, None);
    menu.append_items(&[&show, &refresh, &PredefinedMenuItem::separator(), &quit])?;
    Ok(TrayIconBuilder::new()
        .with_id(format!("aiu-tray-{}", std::process::id()))
        .with_tooltip("AIU — account usage")
        .with_menu(Box::new(menu))
        .with_icon(icon()?)
        .with_icon_as_template(cfg!(target_os = "macos"))
        .with_menu_on_left_click(cfg!(target_os = "macos"))
        .build()?)
}

#[cfg(target_os = "linux")]
fn ensure_status_notifier_host() -> Result<()> {
    use gio::prelude::DBusProxyExt;
    use glib::prelude::ToVariant;
    use gtk::{gio, glib};

    let proxy = gio::DBusProxy::for_bus_sync(
        gio::BusType::Session,
        gio::DBusProxyFlags::DO_NOT_AUTO_START,
        None,
        "org.kde.StatusNotifierWatcher",
        "/StatusNotifierWatcher",
        "org.freedesktop.DBus.Properties",
        None::<&gio::Cancellable>,
    )
    .map_err(|error| anyhow::anyhow!("StatusNotifier host unavailable: {error}"))?;
    let parameters = (
        "org.kde.StatusNotifierWatcher",
        "IsStatusNotifierHostRegistered",
    )
        .to_variant();
    let result = proxy
        .call_sync(
            "Get",
            Some(&parameters),
            gio::DBusCallFlags::NONE,
            1000,
            None::<&gio::Cancellable>,
        )
        .map_err(|error| anyhow::anyhow!("StatusNotifier host unavailable: {error}"))?;
    let registered = result
        .child_value(0)
        .get::<glib::Variant>()
        .and_then(|value| value.get::<bool>())
        .unwrap_or(false);
    if registered {
        Ok(())
    } else {
        Err(anyhow::anyhow!("no StatusNotifier host is registered"))
    }
}

fn icon() -> Result<Icon> {
    // Three usage bars, drawn at 2x for crisp high-DPI tray and menu bar rendering.
    let mut pixels = vec![0; 32 * 32 * 4];
    for (left, top, bottom) in [(4, 18, 28), (13, 10, 28), (22, 4, 28)] {
        for y in top..bottom {
            for x in left..left + 6 {
                let offset = (y * 32 + x) * 4;
                pixels[offset..offset + 4].copy_from_slice(&[133, 203, 193, 255]);
            }
        }
    }
    Ok(Icon::from_rgba(pixels, 32, 32)?)
}
