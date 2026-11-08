//! Process memory reading. Node's V8 heap fields have no Rust equivalent, so
//! only RSS is reported (real on Linux, zero elsewhere); callers fill the rest
//! with honest zeros.

pub fn rss_bytes() -> u64 {
    #[cfg(target_os = "linux")]
    {
        let Ok(raw) = std::fs::read_to_string("/proc/self/status") else {
            return 0;
        };
        for line in raw.lines() {
            if let Some(rest) = line.strip_prefix("VmRSS:") {
                let kb = rest
                    .split_whitespace()
                    .next()
                    .and_then(|v| v.parse::<u64>().ok())
                    .unwrap_or(0);
                return kb.saturating_mul(1024);
            }
        }
        0
    }
    #[cfg(not(target_os = "linux"))]
    {
        0
    }
}
