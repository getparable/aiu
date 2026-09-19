use crate::{
    config::{Config, StoreMode},
    model::{Index, Record},
};
use anyhow::{Context, Result, bail};
use fs2::FileExt;
use serde::{Serialize, de::DeserializeOwned};
use sha2::{Digest, Sha256};
use std::{
    fs::{self, File, OpenOptions},
    io::Write,
    path::{Path, PathBuf},
    thread,
    time::{Duration, Instant},
};
use tempfile::NamedTempFile;

const PENDING_PREFIX: &str = "__pending__:";

pub fn read_json<T: DeserializeOwned>(path: &Path) -> Result<Option<T>> {
    match fs::read(path) {
        Ok(bytes) => {
            Ok(Some(serde_json::from_slice(&bytes).with_context(|| {
                format!("invalid JSON in {}", path.display())
            })?))
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(e).with_context(|| format!("reading {}", path.display())),
    }
}

fn private_dir(path: &Path) -> Result<()> {
    if let Some(parent) = path.parent() {
        #[cfg(unix)]
        let missing = !parent.exists();
        fs::create_dir_all(parent)?;
        #[cfg(unix)]
        if missing {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(parent, fs::Permissions::from_mode(0o700)).ok();
        }
    }
    Ok(())
}

pub fn write_json<T: Serialize>(path: &Path, value: &T) -> Result<()> {
    private_dir(path)?;
    let data = serde_json::to_vec_pretty(value)?;
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    let mut tmp = NamedTempFile::new_in(parent)?;
    tmp.write_all(&data)?;
    tmp.write_all(b"\n")?;
    tmp.flush()?;
    tmp.as_file().sync_all()?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tmp.as_file()
            .set_permissions(fs::Permissions::from_mode(0o600))?;
    }
    tmp.persist(path)
        .map_err(|e| anyhow::anyhow!("replacing {}: {}", path.display(), e.error))?;
    Ok(())
}

pub struct FileLock {
    file: File,
}
impl Drop for FileLock {
    fn drop(&mut self) {
        let _ = fs2::FileExt::unlock(&self.file);
    }
}
pub fn lock(path: &Path) -> Result<FileLock> {
    private_dir(path)?;
    let file = OpenOptions::new()
        .create(true)
        .read(true)
        .write(true)
        .truncate(false)
        .open(path)?;
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        match file.try_lock_exclusive() {
            Ok(()) => return Ok(FileLock { file }),
            Err(e) if Instant::now() < deadline => {
                let _ = e;
                thread::sleep(Duration::from_millis(25));
            }
            Err(e) => bail!("lock timeout for {}: {e}", path.display()),
        }
    }
}

/// Claim optional background work without making interactive commands wait.
pub fn try_lock(path: &Path) -> Result<Option<FileLock>> {
    private_dir(path)?;
    let file = OpenOptions::new()
        .create(true)
        .read(true)
        .write(true)
        .truncate(false)
        .open(path)?;
    match file.try_lock_exclusive() {
        Ok(()) => Ok(Some(FileLock { file })),
        Err(error) if error.raw_os_error() == fs2::lock_contended_error().raw_os_error() => {
            Ok(None)
        }
        Err(error) => Err(error.into()),
    }
}

pub struct Store {
    config: Config,
}
impl Store {
    pub fn new(config: Config) -> Self {
        Self { config }
    }
    pub fn account_lock(&self, key: &str) -> Result<FileLock> {
        let digest = format!("{:x}", Sha256::digest(key.as_bytes()));
        lock(&self.config.dir.join(format!("account-{digest}.lock")))
    }
    fn token_path(&self) -> PathBuf {
        match self.config.store_mode {
            StoreMode::Native => self.config.dir.join("tokens.dpapi"),
            StoreMode::File => self.config.dir.join("tokens.json"),
        }
    }
    fn with_tokens<T>(
        &self,
        f: impl FnOnce(&mut serde_json::Map<String, serde_json::Value>) -> Result<T>,
    ) -> Result<T> {
        let path = self.token_path();
        let lock_path = path.with_extension("json.lock");
        let _guard = lock(&lock_path)?;
        let mut value = match self.read_tokens()? {
            Some(v) => v,
            None => serde_json::Map::new(),
        };
        let out = f(&mut value)?;
        self.write_tokens(&value)?;
        Ok(out)
    }
    fn read_tokens(&self) -> Result<Option<serde_json::Map<String, serde_json::Value>>> {
        match self.config.store_mode {
            StoreMode::File => read_json(&self.token_path()),
            StoreMode::Native => native_read(&self.token_path()),
        }
    }
    fn write_tokens(&self, value: &serde_json::Map<String, serde_json::Value>) -> Result<()> {
        match self.config.store_mode {
            StoreMode::File => write_json(&self.token_path(), value),
            StoreMode::Native => native_write(&self.token_path(), value),
        }
    }
    pub fn load_index(&self) -> Result<Index> {
        Ok(read_json(&self.config.index_file())?.unwrap_or_default())
    }
    pub fn save_index(&self, index: &Index) -> Result<()> {
        write_json(&self.config.index_file(), index)
    }
    pub fn get(&self, key: &str) -> Result<Option<Record>> {
        Ok(self
            .read_tokens()?
            .and_then(|m| m.get(key).cloned())
            .map(serde_json::from_value)
            .transpose()?)
    }
    pub fn set(&self, record: &Record) -> Result<()> {
        self.with_tokens(|m| {
            m.insert(record.key(), serde_json::to_value(record)?);
            Ok(())
        })
    }
    pub fn delete(&self, key: &str) -> Result<()> {
        self.with_tokens(|m| {
            m.remove(key);
            Ok(())
        })
    }

    /// Save a credential that has been refreshed but has not yet been indexed.
    /// It lives in the same protected token container as normal records.
    pub fn set_pending(
        &self,
        provider: crate::model::Provider,
        spent_refresh: &str,
        record: &Record,
    ) -> Result<()> {
        let recovery_id = if spent_refresh.is_empty() {
            format!(
                "access:{:x}",
                Sha256::digest(record.access_token.as_bytes())
            )
        } else {
            spent_refresh.to_owned()
        };
        let lineage = format!("{provider}:{recovery_id}");
        let digest = format!("{:x}", Sha256::digest(lineage.as_bytes()));
        self.with_tokens(|m| {
            // Keep every earlier spent-token alias pointed at the newest pair,
            // including a retry that rotates an already-pending credential.
            for (key, value) in m.iter_mut() {
                if key.starts_with(&format!("{PENDING_PREFIX}{provider}:")) {
                    let (_, pending) = Self::decode_pending(value.clone())?;
                    if !spent_refresh.is_empty() && pending.refresh_token == spent_refresh {
                        value["record"] = serde_json::to_value(record)?;
                    }
                }
            }
            m.insert(
                format!("{PENDING_PREFIX}{provider}:{digest}"),
                serde_json::json!({ "spent_refresh": recovery_id, "record": record }),
            );
            Ok(())
        })
    }

    pub fn pending(&self, provider: crate::model::Provider) -> Result<Option<(String, Record)>> {
        let values = self.read_tokens()?.unwrap_or_default();
        let Some(value) = values.into_iter().find_map(|(key, value)| {
            key.starts_with(&format!("{PENDING_PREFIX}{provider}:"))
                .then_some(value)
        }) else {
            return Ok(None);
        };
        Self::decode_pending(value).map(Some)
    }

    pub fn pending_matching(
        &self,
        provider: crate::model::Provider,
        access: &str,
        refresh: &str,
    ) -> Result<Option<(String, Record)>> {
        let values = self.read_tokens()?.unwrap_or_default();
        for (key, value) in values {
            if !key.starts_with(&format!("{PENDING_PREFIX}{provider}:")) {
                continue;
            }
            let decoded = Self::decode_pending(value)?;
            if (!refresh.is_empty() && (decoded.0 == refresh || decoded.1.refresh_token == refresh))
                || (!access.is_empty() && decoded.1.access_token == access)
            {
                return Ok(Some(decoded));
            }
        }
        Ok(None)
    }

    fn decode_pending(value: serde_json::Value) -> Result<(String, Record)> {
        let spent = value
            .get("spent_refresh")
            .and_then(serde_json::Value::as_str)
            .unwrap_or_default()
            .to_owned();
        let record = serde_json::from_value(
            value
                .get("record")
                .cloned()
                .context("pending credential is missing its record")?,
        )?;
        Ok((spent, record))
    }

    pub fn clear_pending(
        &self,
        provider: crate::model::Provider,
        spent_refresh: &str,
    ) -> Result<()> {
        let digest = format!(
            "{:x}",
            Sha256::digest(format!("{provider}:{spent_refresh}").as_bytes())
        );
        let key = format!("{PENDING_PREFIX}{provider}:{digest}");
        self.with_tokens(|m| {
            if let Some(value) = m.get(&key) {
                let (_, committed) = Self::decode_pending(value.clone())?;
                let mut aliases = Vec::new();
                for (candidate, value) in m.iter() {
                    if candidate.starts_with(&format!("{PENDING_PREFIX}{provider}:")) {
                        let (_, record) = Self::decode_pending(value.clone())?;
                        if record.access_token == committed.access_token
                            && record.refresh_token == committed.refresh_token
                        {
                            aliases.push(candidate.clone());
                        }
                    }
                }
                for alias in aliases {
                    m.remove(&alias);
                }
            }
            Ok(())
        })
    }
    pub fn records(&self) -> Result<Vec<Record>> {
        let idx = self.load_index()?;
        let tokens = self.read_tokens()?.unwrap_or_default();
        idx.accounts
            .into_iter()
            .map(|e| {
                let mut r = tokens
                    .get(&e.key())
                    .cloned()
                    .map(serde_json::from_value)
                    .transpose()?
                    .unwrap_or_else(|| Record {
                        provider: e.provider,
                        email: e.email.clone(),
                        label: e.label.clone(),
                        org_uuid: e.org.clone(),
                        org_name: e.org_name.clone(),
                        missing: true,
                        ..Default::default()
                    });
                r.provider = e.provider;
                r.email = e.email;
                r.label = e.label;
                r.org_uuid = e.org;
                r.org_name = e.org_name;
                Ok(r)
            })
            .collect()
    }
}

#[cfg(not(any(windows, target_os = "macos")))]
fn native_read(_: &Path) -> Result<Option<serde_json::Map<String, serde_json::Value>>> {
    bail!("native credential storage is unavailable; set AIU_STORE=file")
}
#[cfg(not(any(windows, target_os = "macos")))]
fn native_write(_: &Path, _: &serde_json::Map<String, serde_json::Value>) -> Result<()> {
    bail!("native credential storage is unavailable; set AIU_STORE=file")
}

#[cfg(target_os = "macos")]
fn native_read(path: &Path) -> Result<Option<serde_json::Map<String, serde_json::Value>>> {
    use security_framework::passwords::get_generic_password;
    let account = keychain_account(path);
    match get_generic_password("aiu-rs", &account) {
        Ok(bytes) => Ok(Some(serde_json::from_slice(&bytes)?)),
        Err(e) if e.code() == -25300 => Ok(None),
        Err(e) => Err(e.into()),
    }
}
#[cfg(target_os = "macos")]
fn native_write(path: &Path, value: &serde_json::Map<String, serde_json::Value>) -> Result<()> {
    use security_framework::passwords::set_generic_password;
    set_generic_password(
        "aiu-rs",
        &keychain_account(path),
        &serde_json::to_vec(value)?,
    )?;
    Ok(())
}
#[cfg(target_os = "macos")]
fn keychain_account(path: &Path) -> String {
    use sha2::{Digest, Sha256};
    format!(
        "aiu-rs-{:x}",
        Sha256::digest(
            path.parent()
                .unwrap_or(Path::new("."))
                .to_string_lossy()
                .as_bytes()
        )
    )
}

#[cfg(windows)]
#[link(name = "kernel32")]
unsafe extern "system" {
    fn LocalFree(h: *mut core::ffi::c_void) -> *mut core::ffi::c_void;
}
#[cfg(windows)]
struct DpapiBlob(*mut u8);
#[cfg(windows)]
impl Drop for DpapiBlob {
    fn drop(&mut self) {
        if !self.0.is_null() {
            unsafe {
                LocalFree(self.0.cast());
            }
        }
    }
}
#[cfg(windows)]
fn native_read(path: &Path) -> Result<Option<serde_json::Map<String, serde_json::Value>>> {
    use windows_sys::Win32::Security::Cryptography::*;
    let bytes = match fs::read(path) {
        Ok(v) => v,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e.into()),
    };
    unsafe {
        let cb = u32::try_from(bytes.len()).context("DPAPI input too large")?;
        let inb = CRYPT_INTEGER_BLOB {
            cbData: cb,
            pbData: bytes.as_ptr() as *mut u8,
        };
        let mut out = std::mem::zeroed();
        if CryptUnprotectData(
            &inb,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            CRYPTPROTECT_UI_FORBIDDEN,
            &mut out,
        ) == 0
        {
            bail!("DPAPI decrypt failed")
        };
        let _owned = DpapiBlob(out.pbData);
        let s = std::slice::from_raw_parts(out.pbData, out.cbData as usize);
        let v = serde_json::from_slice(s)?;
        Ok(Some(v))
    }
}
#[cfg(windows)]
fn native_write(path: &Path, value: &serde_json::Map<String, serde_json::Value>) -> Result<()> {
    use windows_sys::Win32::Security::Cryptography::*;
    private_dir(path)?;
    let bytes = serde_json::to_vec(value)?;
    unsafe {
        let cb = u32::try_from(bytes.len()).context("DPAPI input too large")?;
        let inb = CRYPT_INTEGER_BLOB {
            cbData: cb,
            pbData: bytes.as_ptr() as *mut u8,
        };
        let mut out = std::mem::zeroed();
        if CryptProtectData(
            &inb,
            std::ptr::null(),
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            CRYPTPROTECT_UI_FORBIDDEN,
            &mut out,
        ) == 0
        {
            bail!("DPAPI encrypt failed")
        };
        let _owned = DpapiBlob(out.pbData);
        let data = std::slice::from_raw_parts(out.pbData, out.cbData as usize);
        let mut tmp = NamedTempFile::new_in(path.parent().unwrap())?;
        tmp.write_all(data)?;
        tmp.as_file().sync_all()?;
        tmp.persist(path).map_err(|e| anyhow::anyhow!(e.error))?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::StoreMode;
    use std::fs;
    use tempfile::tempdir;

    fn store() -> (tempfile::TempDir, Store) {
        let d = tempdir().unwrap();
        let mut c = Config::for_home(d.path().into(), d.path().join("cfg"));
        c.store_mode = StoreMode::File;
        (d, Store::new(c))
    }
    #[test]
    fn roundtrip() {
        let (_d, s) = store();
        let r = Record {
            email: "a@b".into(),
            ..Default::default()
        };
        s.set(&r).unwrap();
        assert_eq!(s.get(&r.key()).unwrap().unwrap().email, "a@b");
    }

    #[test]
    fn records_join_index_and_hide_orphans() {
        let (_d, s) = store();
        let r = Record {
            email: "kept@example.com".into(),
            ..Default::default()
        };
        s.set(&r).unwrap();
        let orphan = Record {
            email: "orphan@example.com".into(),
            ..Default::default()
        };
        s.set(&orphan).unwrap();
        s.save_index(&Index {
            version: 1,
            accounts: vec![crate::model::IndexEntry {
                email: r.email.clone(),
                label: "kept".into(),
                provider: r.provider,
                ..Default::default()
            }],
        })
        .unwrap();
        let rs = s.records().unwrap();
        assert_eq!(rs.len(), 1);
        assert_eq!(rs[0].email, "kept@example.com");
        assert!(!rs[0].missing);
        s.delete(&r.key()).unwrap();
        let rs = s.records().unwrap();
        assert_eq!(rs.len(), 1);
        assert!(rs[0].missing);
    }

    #[test]
    fn corrupt_store_is_not_overwritten() {
        let (d, s) = store();
        let path = d.path().join("cfg/tokens.json");
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, b"{corrupt").unwrap();
        assert!(s.get("x").is_err());
        assert_eq!(fs::read(&path).unwrap(), b"{corrupt");
    }

    #[test]
    fn repeated_atomic_replacements_keep_latest() {
        let (_d, s) = store();
        let mut r = Record {
            email: "repeat@example.com".into(),
            ..Default::default()
        };
        for i in 0..20 {
            r.label = i.to_string();
            s.set(&r).unwrap();
        }
        assert_eq!(s.get(&r.key()).unwrap().unwrap().label, "19");
    }

    #[cfg(windows)]
    #[test]
    fn dpapi_roundtrip_large_secret_is_ciphertext() {
        let d = tempdir().unwrap();
        let p = d.path().join("tokens.dpapi");
        let mut m = serde_json::Map::new();
        m.insert("token".into(), serde_json::json!("x".repeat(8192)));
        native_write(&p, &m).unwrap();
        let raw = fs::read(&p).unwrap();
        assert!(!String::from_utf8_lossy(&raw).contains(&"x".repeat(100)));
        assert_eq!(
            native_read(&p).unwrap().unwrap()["token"]
                .as_str()
                .unwrap()
                .len(),
            8192
        );
    }
}
