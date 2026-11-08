//! Table-driven parity fixtures ported from v1 `node:test` suites.
//!
//! The fixtures live in `tests/fixtures/` as JSON so the same inputs/expected
//! outputs can be diffed against v1 without reading Rust. They cover the
//! deterministic pieces where silent drift would break behavior parity:
//! greeting detection, WeChat image batching, and cron/once/workday firing.

use botler::channels::image_batch::{
    BatchAction, BatchDispatch, ImageBatchCoordinator, PendingPiece,
};
use botler::greeting::is_greeting;
use botler::runtime::InboundImage;
use botler::scheduler::cron::next_fire_epoch;
use botler::scheduler::holidays::{HolidayCalendar, HolidayData};
use botler::scheduler::types::ScheduleEntry;
use chrono::DateTime;
use serde::Deserialize;

fn fixture(name: &str) -> String {
    let path = format!("{}/tests/fixtures/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|err| panic!("read {path}: {err}"))
}

#[derive(Deserialize)]
struct GreetingCase {
    input: String,
    greeting: bool,
}

#[test]
fn greeting_fixtures_match() {
    let cases: Vec<GreetingCase> = serde_json::from_str(&fixture("greeting.json")).unwrap();
    assert!(!cases.is_empty());
    for case in cases {
        assert_eq!(
            is_greeting(&case.input),
            case.greeting,
            "greeting({:?})",
            case.input
        );
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct BatchCase {
    name: String,
    window_ms: u64,
    steps: Vec<BatchStep>,
    #[serde(default)]
    flush: Vec<String>,
    expect: Vec<Expect>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct BatchStep {
    sender: String,
    #[serde(default)]
    text: String,
    #[serde(default)]
    images: Vec<u8>,
    #[serde(default)]
    token: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Expect {
    kind: String,
    #[serde(default)]
    text: Option<String>,
    #[serde(default)]
    image_count: Option<usize>,
    #[serde(default)]
    token: Option<String>,
    #[serde(default)]
    id_prefix: Option<String>,
}

impl Expect {
    fn dispatch_parts(&self, context: &str) -> (&str, usize, Option<&str>, &str) {
        assert_eq!(self.kind, "dispatch", "{context}: expected a dispatch");
        (
            self.text
                .as_deref()
                .unwrap_or_else(|| panic!("{context}: text")),
            self.image_count
                .unwrap_or_else(|| panic!("{context}: imageCount")),
            self.token.as_deref(),
            self.id_prefix
                .as_deref()
                .unwrap_or_else(|| panic!("{context}: idPrefix")),
        )
    }
}

fn image(byte: u8) -> InboundImage {
    InboundImage {
        bytes: vec![byte],
        mime_type: "image/jpeg".into(),
        ext: "jpg".into(),
    }
}

fn assert_dispatch(actual: &BatchDispatch, expected: &Expect, context: &str) {
    let (text, image_count, token, id_prefix) = expected.dispatch_parts(context);
    assert_eq!(actual.text, text, "{context}: text");
    assert_eq!(actual.images.len(), image_count, "{context}: image count");
    assert_eq!(actual.context_token.as_deref(), token, "{context}: token");
    assert!(
        actual.id.starts_with(id_prefix),
        "{context}: id {:?} does not start with {id_prefix:?}",
        actual.id
    );
}

#[test]
fn image_batch_fixtures_match() {
    let cases: Vec<BatchCase> = serde_json::from_str(&fixture("image_batch.json")).unwrap();
    assert!(!cases.is_empty());

    for case in cases {
        let mut coordinator = ImageBatchCoordinator::new(case.window_ms);
        let mut expected = case.expect.into_iter();

        for (index, step) in case.steps.iter().enumerate() {
            let action = coordinator.on_message(
                &step.sender,
                PendingPiece {
                    text: step.text.clone(),
                    images: step.images.iter().copied().map(image).collect(),
                    context_token: step.token.clone(),
                },
            );
            let want = expected
                .next()
                .unwrap_or_else(|| panic!("{}: step {index}: missing expectation", case.name));
            match (&action, want.kind.as_str()) {
                (BatchAction::Buffer, "buffer") => {}
                (BatchAction::Dispatch(task), "dispatch") => {
                    assert_dispatch(task, &want, &format!("{}: step {index}", case.name));
                }
                _ => panic!(
                    "{}: step {index}: got {action:?}, expected {want:?}",
                    case.name
                ),
            }
        }

        for sender in &case.flush {
            let flushed = coordinator.flush(sender);
            let want = expected
                .next()
                .unwrap_or_else(|| panic!("{}: flush {sender}: missing expectation", case.name));
            match (flushed, want.kind.as_str()) {
                (Some(task), "dispatch") => {
                    assert_dispatch(&task, &want, &format!("{}: flush {sender}", case.name));
                }
                (None, "none") => {}
                (actual, _) => panic!(
                    "{}: flush {sender}: got {actual:?}, expected {want:?}",
                    case.name
                ),
            }
        }

        assert!(
            expected.next().is_none(),
            "{}: unused expectations",
            case.name
        );
    }
}

#[derive(Deserialize)]
struct CronFixture {
    #[serde(default)]
    calendar: HolidayData,
    cases: Vec<CronCase>,
}

#[derive(Deserialize)]
struct CronCase {
    name: String,
    entry: ScheduleEntry,
    after: String,
    expect: Option<String>,
}

fn epoch_ms(rfc3339: &str, context: &str) -> i64 {
    DateTime::parse_from_rfc3339(rfc3339)
        .unwrap_or_else(|err| panic!("{context}: bad timestamp {rfc3339:?}: {err}"))
        .timestamp_millis()
}

#[test]
fn cron_fixtures_match() {
    let data: CronFixture = serde_json::from_str(&fixture("cron.json")).unwrap();
    assert!(!data.cases.is_empty());
    let calendar = HolidayCalendar::from_data(data.calendar);

    for case in data.cases {
        let after = epoch_ms(&case.after, &case.name);
        let actual = next_fire_epoch(&case.entry, after, &calendar)
            .unwrap_or_else(|err| panic!("{}: {err}", case.name));
        let expected = case
            .expect
            .as_deref()
            .map(|stamp| epoch_ms(stamp, &case.name));
        assert_eq!(actual, expected, "{}", case.name);
    }
}

// ---- Task-log query primitives (shared by the WebUI's /api/logs) ----

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct LogQueryFixture {
    day_files: Vec<DayFileCase>,
    schedule_ids: Vec<ScheduleIdCase>,
    query_matches: Vec<QueryMatchCase>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct DayFileCase {
    name: String,
    valid: bool,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ScheduleIdCase {
    task_id: String,
    expect: Option<String>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct QueryMatchCase {
    name: String,
    log: serde_json::Value,
    query: QuerySpec,
    matches: bool,
}

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase")]
struct QuerySpec {
    project: Option<String>,
    source: Option<String>,
    schedule_id: Option<String>,
    phase: Option<String>,
    q: Option<String>,
}

#[test]
fn log_query_fixtures_match() {
    use botler::task_log::record::TaskLog;
    use botler::task_log::store::{
        is_day_file, matches_log_query, schedule_id_from_task_id, LogQuery,
    };

    let data: LogQueryFixture = serde_json::from_str(&fixture("log_query.json")).unwrap();
    assert!(!data.day_files.is_empty());
    assert!(!data.schedule_ids.is_empty());
    assert!(!data.query_matches.is_empty());

    for case in data.day_files {
        assert_eq!(
            is_day_file(&case.name),
            case.valid,
            "is_day_file({:?})",
            case.name
        );
    }
    for case in data.schedule_ids {
        assert_eq!(
            schedule_id_from_task_id(&case.task_id),
            case.expect,
            "schedule_id_from_task_id({:?})",
            case.task_id
        );
    }
    for case in data.query_matches {
        let log: TaskLog = serde_json::from_value(case.log).unwrap();
        let query = LogQuery {
            project: case.query.project,
            source: case.query.source,
            schedule_id: case.query.schedule_id,
            phase: case.query.phase,
            q: case.query.q,
            ..LogQuery::default()
        };
        assert_eq!(
            matches_log_query(&log, &query),
            case.matches,
            "{}",
            case.name
        );
    }
}
