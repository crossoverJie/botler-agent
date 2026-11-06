//! Zero-dependency (beyond chrono) cron compiler, next-fire calculator, and
//! IANA timezone conversion. Semantics mirror v1's `scheduler/cron.ts`.

use anyhow::{bail, Result};
use chrono::{Datelike, Duration, NaiveDate, NaiveDateTime, NaiveTime, TimeZone, Timelike};
use chrono_tz::Tz;
use std::str::FromStr;

use crate::scheduler::holidays::HolidayCalendar;
use crate::scheduler::types::ScheduleEntry;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CompiledCron {
    pub minute: Vec<u32>,
    pub hour: Vec<u32>,
    pub dom: Vec<u32>,
    pub month: Vec<u32>,
    pub dow: Vec<u32>,
}

pub fn is_valid_timezone(tz: &str) -> bool {
    Tz::from_str(tz).is_ok()
}

fn max_day_of_month(month: u32) -> u32 {
    match month {
        2 => 29,
        4 | 6 | 9 | 11 => 30,
        _ => 31,
    }
}

fn compile_field(spec: &str, min: u32, max: u32, is_dow: bool) -> Result<Vec<u32>> {
    let mut out = std::collections::BTreeSet::new();
    for part_raw in spec.split(',') {
        let part = part_raw.trim();
        if part.is_empty() {
            continue;
        }
        let (range, step) = match part.split_once('/') {
            Some((r, s)) => {
                let step: u32 = s
                    .parse()
                    .ok()
                    .filter(|n| *n > 0)
                    .ok_or_else(|| anyhow::anyhow!("invalid step in \"{part}\""))?;
                (r, step)
            }
            None => (part, 1),
        };
        let (lo, hi) = if range == "*" || range == "?" {
            (min, max)
        } else if let Some((a, b)) = range.split_once('-') {
            let lo: u32 = a
                .parse()
                .map_err(|_| anyhow::anyhow!("invalid range \"{range}\""))?;
            let hi: u32 = b
                .parse()
                .map_err(|_| anyhow::anyhow!("invalid range \"{range}\""))?;
            (lo, hi)
        } else {
            let v: u32 = range
                .parse()
                .map_err(|_| anyhow::anyhow!("invalid field \"{range}\""))?;
            (v, v)
        };
        if lo < min || hi > max {
            bail!("value out of range [{min},{max}] in \"{part}\"");
        }
        if lo > hi {
            bail!("range start > end in \"{part}\"");
        }
        let mut v = lo;
        while v <= hi {
            out.insert(if is_dow && v == 7 { 0 } else { v });
            v += step;
        }
    }
    Ok(out.into_iter().collect())
}

fn assert_day_reachable(c: &CompiledCron, expr: &str) -> Result<()> {
    let reachable = c
        .month
        .iter()
        .any(|m| c.dom.iter().any(|d| *d <= max_day_of_month(*m)));
    if !reachable {
        bail!(
            "cron \"{expr}\" has no valid date: day-of-month {:?} never occurs in month {:?}",
            c.dom,
            c.month
        );
    }
    Ok(())
}

pub fn compile_cron(expr: &str) -> Result<CompiledCron> {
    let fields: Vec<&str> = expr.split_whitespace().collect();
    if fields.len() != 5 {
        bail!("cron requires 5 fields, got {}: \"{expr}\"", fields.len());
    }
    let c = CompiledCron {
        minute: compile_field(fields[0], 0, 59, false)?,
        hour: compile_field(fields[1], 0, 23, false)?,
        dom: compile_field(fields[2], 1, 31, false)?,
        month: compile_field(fields[3], 1, 12, false)?,
        dow: compile_field(fields[4], 0, 7, true)?,
    };
    assert_day_reachable(&c, expr)?;
    Ok(c)
}

fn neutralize_workday_cron(cron: &str) -> Result<String> {
    let f: Vec<&str> = cron.split_whitespace().collect();
    if f.len() != 5 {
        bail!("cron requires 5 fields, got {}: \"{cron}\"", f.len());
    }
    Ok(format!("{} {} * * *", f[0], f[1]))
}

pub fn compile_workday_cron(cron: &str) -> Result<CompiledCron> {
    compile_cron(&neutralize_workday_cron(cron)?)
}

fn interval_to_cron(s: &str) -> Result<String> {
    let (num, unit) = match s.char_indices().last() {
        Some((i, c)) if matches!(c, 'm' | 'h' | 'd') => (&s[..i], c),
        _ => bail!("invalid interval \"{s}\""),
    };
    let n: u32 = num
        .parse()
        .map_err(|_| anyhow::anyhow!("invalid interval \"{s}\""))?;
    if n < 1 {
        bail!("invalid interval \"{s}\": step must be >= 1");
    }
    match unit {
        'm' => {
            if n > 59 {
                bail!("invalid interval \"{s}\": minutes must be 1-59, use \"1h\"/\"2h\" or a cron for longer");
            }
            Ok(format!("*/{n} * * * *"))
        }
        'h' => {
            if n > 24 {
                bail!(
                    "invalid interval \"{s}\": hours must be 1-24, use \"1d\" or a cron for longer"
                );
            }
            Ok(format!("0 */{n} * * *"))
        }
        _ => {
            if n != 1 {
                bail!("invalid interval \"{s}\": a 5-field cron cannot express \"every {n} days\"; use cron/at instead");
            }
            Ok("0 0 * * *".into())
        }
    }
}

fn parse_hm(s: &str) -> Option<(u32, u32)> {
    let (h, m) = s.split_once(':')?;
    let h: u32 = h.parse().ok()?;
    let m: u32 = m.parse().ok()?;
    if h > 23 || m > 59 {
        return None;
    }
    Some((h, m))
}

fn at_to_cron(s: &str) -> Result<String> {
    let (h, m) = parse_hm(s).ok_or_else(|| anyhow::anyhow!("invalid at \"{s}\""))?;
    Ok(format!("{m} {h} * * *"))
}

/// Parse a `once` trigger to epoch millis. Accepted forms:
/// absolute with `Z` or `+HH:MM` (also `+HHMM`), or a naive local datetime
/// interpreted in `tz`.
pub fn parse_once_epoch(s: &str, tz: &str) -> Option<i64> {
    let trimmed = s.trim();
    if trimmed.is_empty() {
        return None;
    }
    // Validate the calendar date prefix ourselves: chrono's RFC 3339 parser
    // rejects impossible dates, but this also covers the naive branch below.
    if trimmed.len() < 10 {
        return None;
    }
    NaiveDate::parse_from_str(&trimmed[..10], "%Y-%m-%d").ok()?;

    let offset_idx = trimmed
        .char_indices()
        .skip(10)
        .filter(|(_, c)| *c == '+' || *c == '-')
        .map(|(i, _)| i)
        .last();
    let has_offset = trimmed.ends_with('Z') || trimmed.ends_with('z') || offset_idx.is_some();
    if has_offset {
        let mut normalized = trimmed.replace(' ', "T");
        if let Some(idx) = offset_idx {
            let (head, off) = normalized.split_at(idx);
            let sign = &off[..1];
            let digits: String = off[1..].chars().filter(|c| *c != ':').collect();
            if digits.len() == 4 && digits.chars().all(|c| c.is_ascii_digit()) {
                normalized = format!("{head}{sign}{}:{}", &digits[..2], &digits[2..]);
            }
        }
        let normalized = if normalized.ends_with('Z') || normalized.ends_with('z') {
            format!("{}Z", &normalized[..normalized.len() - 1])
        } else {
            normalized
        };
        return chrono::DateTime::parse_from_rfc3339(&normalized)
            .ok()
            .map(|d| d.timestamp_millis());
    }

    // Naive local form: `YYYY-MM-DD[T ]HH:MM[:SS]`.
    let normalized = trimmed.replace(' ', "T");
    let (date_part, time_part) = normalized.split_once('T')?;
    let date = NaiveDate::parse_from_str(date_part, "%Y-%m-%d").ok()?;
    let parts: Vec<&str> = time_part.split(':').collect();
    if parts.len() < 2 || parts.len() > 3 {
        return None;
    }
    let hour: u32 = parts[0].parse().ok()?;
    let minute: u32 = parts[1].parse().ok()?;
    let second: u32 = if parts.len() == 3 {
        parts[2].parse().ok()?
    } else {
        0
    };
    if hour > 23 || minute > 59 || second > 59 {
        return None;
    }
    let time = NaiveTime::from_hms_opt(hour, minute, second)?;
    let naive = NaiveDateTime::new(date, time);
    wall_to_epoch(&naive, tz)
}

fn tz_from_name(tz: &str) -> Option<Tz> {
    Tz::from_str(tz).ok()
}

/// Naive wall time -> epoch millis in `tz`.
pub fn wall_to_epoch(wall: &NaiveDateTime, tz: &str) -> Option<i64> {
    let tz = tz_from_name(tz)?;
    tz.from_local_datetime(wall)
        .earliest()
        .map(|dt| dt.timestamp_millis())
}

/// Epoch millis -> naive wall time in `tz`.
pub fn epoch_to_wall(epoch_ms: i64, tz: &str) -> Option<NaiveDateTime> {
    let tz = tz_from_name(tz)?;
    tz.timestamp_millis_opt(epoch_ms)
        .single()
        .map(|dt| dt.naive_local())
}

/// Compile cron/interval/at (not `once`) into a cron.
pub fn compile_schedule(e: &ScheduleEntry) -> Result<CompiledCron> {
    if let Some(cron) = e.cron.as_deref().filter(|s| !s.trim().is_empty()) {
        compile_cron(cron)
    } else if let Some(interval) = e.interval.as_deref().filter(|s| !s.trim().is_empty()) {
        compile_cron(&interval_to_cron(interval.trim())?)
    } else if let Some(at) = e.at.as_deref().filter(|s| !s.trim().is_empty()) {
        compile_cron(&at_to_cron(at.trim())?)
    } else {
        bail!("schedule must specify one of cron/interval/at (or once for one-shot)");
    }
}

fn day_matches(c: &CompiledCron, date: NaiveDate) -> bool {
    let dom_ok = c.dom.contains(&date.day());
    let dow = date.weekday().num_days_from_sunday();
    c.dow.contains(&dow) && dom_ok
}

fn first_time_of_day(c: &CompiledCron, floor_min: u32) -> Option<u32> {
    for h in &c.hour {
        if h * 60 + 59 < floor_min {
            continue;
        }
        for m in &c.minute {
            let tod = h * 60 + m;
            if tod >= floor_min {
                return Some(tod);
            }
        }
    }
    None
}

/// Next wall-clock minute strictly after `after` satisfying the cron.
pub fn next_cron_wall(c: &CompiledCron, after: NaiveDateTime) -> Result<NaiveDateTime> {
    let base = after
        .with_second(0)
        .and_then(|d| d.with_nanosecond(0))
        .ok_or_else(|| anyhow::anyhow!("invalid base time"))?;
    let mut cur = base + Duration::minutes(1);
    let mut floor_min = cur.hour() * 60 + cur.minute();
    let mut guard = 0;
    while guard < 5 * 366 {
        guard += 1;
        let date = cur.date();
        if !c.month.contains(&date.month()) {
            cur = first_of_next_month(date)?;
            floor_min = 0;
            continue;
        }
        if day_matches(c, date) {
            if let Some(tod) = first_time_of_day(c, floor_min) {
                let time = NaiveTime::from_hms_opt(tod / 60, tod % 60, 0).unwrap();
                return Ok(NaiveDateTime::new(date, time));
            }
        }
        cur = NaiveDateTime::new(date + Duration::days(1), NaiveTime::MIN);
        floor_min = 0;
    }
    bail!("nextCronWall: no match found within 5 years")
}

fn first_of_next_month(date: NaiveDate) -> Result<NaiveDateTime> {
    let (y, m) = if date.month() == 12 {
        (date.year() + 1, 1)
    } else {
        (date.year(), date.month() + 1)
    };
    let d = NaiveDate::from_ymd_opt(y, m, 1).ok_or_else(|| anyhow::anyhow!("invalid month"))?;
    Ok(NaiveDateTime::new(d, NaiveTime::MIN))
}

fn workday_time_fields(e: &ScheduleEntry) -> Result<(Vec<u32>, Vec<u32>)> {
    if let Some(cron) = e.cron.as_deref() {
        let c = compile_workday_cron(cron)?;
        Ok((c.hour, c.minute))
    } else {
        let c = compile_schedule(e)?;
        Ok((c.hour, c.minute))
    }
}

pub fn next_workday_fire_epoch(
    e: &ScheduleEntry,
    after_ms: i64,
    calendar: &HolidayCalendar,
) -> Result<Option<i64>> {
    let tz = e.timezone_or_default();
    let (hours, minutes) = workday_time_fields(e)?;
    let mut times: Vec<(u32, u32)> = Vec::new();
    for h in &hours {
        for m in &minutes {
            times.push((*h, *m));
        }
    }
    times.sort_by_key(|(h, m)| h * 60 + m);

    let start = epoch_to_wall(after_ms, tz).ok_or_else(|| anyhow::anyhow!("invalid timezone"))?;
    for i in 0..=366 * 3 {
        let date = start.date() + Duration::days(i);
        if !calendar.is_legal_workday(date) {
            continue;
        }
        for (h, m) in &times {
            let time = NaiveTime::from_hms_opt(*h, *m, 0).unwrap();
            let naive = NaiveDateTime::new(date, time);
            let Some(cand) = wall_to_epoch(&naive, tz) else {
                continue;
            };
            if cand <= after_ms {
                continue;
            }
            let final_ms = if e.silent_hours.is_some() {
                apply_silent_hours(e, cand, tz)
            } else {
                cand
            };
            let Some(final_wall) = epoch_to_wall(final_ms, tz) else {
                continue;
            };
            if !calendar.is_legal_workday(final_wall.date()) {
                continue;
            }
            return Ok(Some(final_ms));
        }
    }
    Ok(None)
}

/// Next fire strictly after `after_ms`; `None` means the entry is inert
/// (a `once` entry whose instant has passed).
pub fn next_fire_epoch(
    e: &ScheduleEntry,
    after_ms: i64,
    calendar: &HolidayCalendar,
) -> Result<Option<i64>> {
    if let Some(once) = e.once.as_deref().filter(|s| !s.trim().is_empty()) {
        let t = parse_once_epoch(once, e.timezone_or_default())
            .ok_or_else(|| anyhow::anyhow!("schedule \"{}\" invalid once \"{once}\"", e.id))?;
        return Ok(if t > after_ms { Some(t) } else { None });
    }
    if e.is_workday_mode() {
        return next_workday_fire_epoch(e, after_ms, calendar);
    }
    let cron = compile_schedule(e)?;
    let tz = e.timezone_or_default();
    let wall = epoch_to_wall(after_ms, tz).ok_or_else(|| anyhow::anyhow!("invalid timezone"))?;
    let next_wall = next_cron_wall(&cron, wall)?;
    let next = wall_to_epoch(&next_wall, tz).ok_or_else(|| anyhow::anyhow!("invalid timezone"))?;
    Ok(Some(if e.silent_hours.is_some() {
        apply_silent_hours(e, next, tz)
    } else {
        next
    }))
}

fn apply_silent_hours(e: &ScheduleEntry, epoch_ms: i64, tz: &str) -> i64 {
    let Some(sh) = e.silent_hours.as_ref() else {
        return epoch_ms;
    };
    let Some((fh, fm)) = parse_hm(&sh.from) else {
        return epoch_ms;
    };
    let Some((th, tm)) = parse_hm(&sh.to) else {
        return epoch_ms;
    };
    let from_min = fh * 60 + fm;
    let to_min = th * 60 + tm;
    if from_min == to_min {
        return epoch_ms;
    }
    let mut cur = epoch_ms;
    for _ in 0..366 {
        let Some(w) = epoch_to_wall(cur, tz) else {
            return epoch_ms;
        };
        let cm = w.hour() * 60 + w.minute();
        let in_window = if from_min < to_min {
            cm >= from_min && cm < to_min
        } else {
            cm >= from_min || cm < to_min
        };
        if !in_window {
            return cur;
        }
        let target_date = if from_min < to_min {
            w.date()
        } else {
            w.date() + Duration::days(1)
        };
        let time = NaiveTime::from_hms_opt(th, tm, 0).unwrap();
        let Some(next) = wall_to_epoch(&NaiveDateTime::new(target_date, time), tz) else {
            return epoch_ms;
        };
        cur = next;
    }
    epoch_ms
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::scheduler::holidays::HolidayCalendar;
    use crate::scheduler::types::{ScheduleEntry, SilentHours};
    use std::collections::HashMap;

    fn entry(trigger: (&str, &str)) -> ScheduleEntry {
        let mut e = ScheduleEntry {
            id: "t".into(),
            enabled: true,
            cron: None,
            interval: None,
            at: None,
            once: None,
            timezone: "Asia/Shanghai".into(),
            message: Some("x".into()),
            script: None,
            script_args: None,
            commit: None,
            project: None,
            retry: None,
            silent_hours: None,
            holiday_mode: None,
            recipient: None,
            origin: None,
        };
        match trigger.0 {
            "cron" => e.cron = Some(trigger.1.into()),
            "interval" => e.interval = Some(trigger.1.into()),
            "at" => e.at = Some(trigger.1.into()),
            "once" => e.once = Some(trigger.1.into()),
            _ => unreachable!(),
        }
        e
    }

    fn empty_calendar() -> HolidayCalendar {
        HolidayCalendar::from_data(HashMap::new())
    }

    #[test]
    fn compiles_supported_cron_forms() {
        assert_eq!(
            compile_cron("*/5 * * * *").unwrap().minute,
            vec![0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55]
        );
        assert_eq!(compile_cron("0 8-10 * * 1-5").unwrap().hour, vec![8, 9, 10]);
        assert_eq!(compile_cron("0 0 * * 7").unwrap().dow, vec![0]);
        assert!(compile_cron("0 0 30 2 *").is_err());
        assert!(compile_cron("60 0 * * *").is_err());
        assert!(compile_cron("0 0 * *").is_err());
    }

    #[test]
    fn interval_conversion_rejects_out_of_range() {
        assert_eq!(interval_to_cron("5m").unwrap(), "*/5 * * * *");
        assert_eq!(interval_to_cron("2h").unwrap(), "0 */2 * * *");
        assert_eq!(interval_to_cron("1d").unwrap(), "0 0 * * *");
        assert!(interval_to_cron("90m").is_err());
        assert!(interval_to_cron("2d").is_err());
    }

    #[test]
    fn next_fire_is_strictly_after() {
        let e = entry(("cron", "0 8 * * *"));
        // 2026-11-03 07:30 +08:00
        let after = wall_to_epoch(
            &NaiveDate::from_ymd_opt(2026, 11, 3)
                .unwrap()
                .and_hms_opt(7, 30, 0)
                .unwrap(),
            "Asia/Shanghai",
        )
        .unwrap();
        let next = next_fire_epoch(&e, after, &empty_calendar())
            .unwrap()
            .unwrap();
        let wall = epoch_to_wall(next, "Asia/Shanghai").unwrap();
        assert_eq!(wall.date(), NaiveDate::from_ymd_opt(2026, 11, 3).unwrap());
        assert_eq!((wall.hour(), wall.minute()), (8, 0));
    }

    #[test]
    fn once_fires_once_then_is_inert() {
        let mut e = entry(("once", "2026-11-03T22:00:00+08:00"));
        let before = wall_to_epoch(
            &NaiveDate::from_ymd_opt(2026, 11, 3)
                .unwrap()
                .and_hms_opt(21, 0, 0)
                .unwrap(),
            "Asia/Shanghai",
        )
        .unwrap();
        let fire = next_fire_epoch(&e, before, &empty_calendar())
            .unwrap()
            .unwrap();
        assert!(next_fire_epoch(&e, fire, &empty_calendar())
            .unwrap()
            .is_none());
        // Naive form is interpreted in the entry timezone.
        e.once = Some("2026-11-03 22:00".into());
        assert_eq!(
            parse_once_epoch("2026-11-03 22:00", "Asia/Shanghai"),
            Some(fire)
        );
    }

    #[test]
    fn rejects_impossible_calendar_dates() {
        assert!(parse_once_epoch("2026-02-30T10:00:00+08:00", "Asia/Shanghai").is_none());
        assert!(parse_once_epoch("2026-02-30 10:00", "Asia/Shanghai").is_none());
    }

    #[test]
    fn silent_hours_defers_to_window_end() {
        let mut e = entry(("cron", "30 23 * * *"));
        e.silent_hours = Some(SilentHours {
            from: "22:00".into(),
            to: "07:00".into(),
        });
        let after = wall_to_epoch(
            &NaiveDate::from_ymd_opt(2026, 11, 3)
                .unwrap()
                .and_hms_opt(23, 0, 0)
                .unwrap(),
            "Asia/Shanghai",
        )
        .unwrap();
        let next = next_fire_epoch(&e, after, &empty_calendar())
            .unwrap()
            .unwrap();
        let wall = epoch_to_wall(next, "Asia/Shanghai").unwrap();
        assert_eq!(wall.date(), NaiveDate::from_ymd_opt(2026, 11, 4).unwrap());
        assert_eq!((wall.hour(), wall.minute()), (7, 0));
    }

    #[test]
    fn workday_mode_skips_holidays_and_allows_makeup_workdays() {
        let mut data = HashMap::new();
        data.insert(
            "2026".to_string(),
            crate::scheduler::holidays::YearData {
                holidays: vec!["2026-11-04".into()],
                workdays: vec!["2026-11-07".into()], // Saturday makeup workday
            },
        );
        let cal = HolidayCalendar::from_data(data);
        let mut e = entry(("cron", "0 8 * * *"));
        e.holiday_mode = Some("workday".into());

        // 2026-11-03 (Tue) 09:00 -> next is 11-05 (Wed, 11-04 is a holiday)
        let after = wall_to_epoch(
            &NaiveDate::from_ymd_opt(2026, 11, 3)
                .unwrap()
                .and_hms_opt(9, 0, 0)
                .unwrap(),
            "Asia/Shanghai",
        )
        .unwrap();
        let wall = epoch_to_wall(
            next_fire_epoch(&e, after, &cal).unwrap().unwrap(),
            "Asia/Shanghai",
        )
        .unwrap();
        assert_eq!(wall.date(), NaiveDate::from_ymd_opt(2026, 11, 5).unwrap());

        // 2026-11-06 (Fri) 09:00 -> 11-07 is a makeup workday despite being Saturday
        let after = wall_to_epoch(
            &NaiveDate::from_ymd_opt(2026, 11, 6)
                .unwrap()
                .and_hms_opt(9, 0, 0)
                .unwrap(),
            "Asia/Shanghai",
        )
        .unwrap();
        let wall = epoch_to_wall(
            next_fire_epoch(&e, after, &cal).unwrap().unwrap(),
            "Asia/Shanghai",
        )
        .unwrap();
        assert_eq!(wall.date(), NaiveDate::from_ymd_opt(2026, 11, 7).unwrap());
    }
}
