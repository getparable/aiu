use anyhow::Result;
use eframe::{Frame, egui};
#[cfg(windows)]
use std::sync::{Arc, Mutex};
use std::{sync::mpsc, thread, time::Duration};

#[derive(Clone)]
pub struct Wake {
    context: egui::Context,
    #[cfg(windows)]
    native: Arc<Mutex<isize>>,
}

impl Wake {
    pub fn new(context: egui::Context, frame: &Frame) -> Result<Self> {
        #[cfg(windows)]
        {
            use raw_window_handle::{HasWindowHandle, RawWindowHandle};

            let hwnd = match frame
                .window_handle()
                .map_err(|error| anyhow::anyhow!("desktop window handle unavailable: {error}"))?
                .as_raw()
            {
                RawWindowHandle::Win32(handle) => handle.hwnd.get(),
                _ => return Err(anyhow::anyhow!("desktop window is not a Win32 window")),
            };
            Ok(Self {
                context,
                native: Arc::new(Mutex::new(hwnd)),
            })
        }

        #[cfg(not(windows))]
        {
            let _ = frame;
            Ok(Self { context })
        }
    }

    pub fn request_repaint(&self) {
        self.context.request_repaint();
        #[cfg(windows)]
        self.request_native_wake();
    }

    #[cfg(windows)]
    fn request_native_wake(&self) {
        // Hold the guard through PostMessage so shutdown cannot invalidate the
        // window handle while a background task is using it.
        let handle = self
            .native
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        if *handle == 0 {
            return;
        }
        let hwnd = *handle as windows_sys::Win32::Foundation::HWND;
        // Winit's normal redraw request can miss hidden or fully occluded
        // windows. Post it explicitly, including for nominally visible windows,
        // to process tray actions and refreshes while another app covers AIU.
        // This handle comes from eframe and is invalidated before its owner exits.
        unsafe {
            let _ = windows_sys::Win32::UI::WindowsAndMessaging::PostMessageW(
                hwnd,
                windows_sys::Win32::UI::WindowsAndMessaging::WM_PAINT,
                0,
                0,
            );
        }
    }
}

pub struct Heartbeat {
    wake: Wake,
    stop: Option<mpsc::Sender<()>>,
    thread: Option<thread::JoinHandle<()>>,
}

impl Heartbeat {
    pub fn new(wake: Wake) -> Self {
        let (stop, stopped) = mpsc::channel();
        let thread_wake = wake.clone();
        let thread = thread::spawn(move || {
            loop {
                match stopped.recv_timeout(Duration::from_secs(1)) {
                    Ok(()) | Err(mpsc::RecvTimeoutError::Disconnected) => break,
                    Err(mpsc::RecvTimeoutError::Timeout) => thread_wake.request_repaint(),
                }
            }
        });
        Self {
            wake,
            stop: Some(stop),
            thread: Some(thread),
        }
    }
}

impl Drop for Heartbeat {
    fn drop(&mut self) {
        let _ = &self.wake;
        #[cfg(windows)]
        {
            *self
                .wake
                .native
                .lock()
                .unwrap_or_else(|error| error.into_inner()) = 0;
        }
        self.stop.take();
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}
