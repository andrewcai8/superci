//! A SuperCI control plane on AWS Lambda (provided.al2023, arm64): GitHub's webhooks arrive through the function's
//! URL; state is one DynamoDB table; secrets (the GitHub App, launchers, dashboard keys) are SecureString parameters
//! under /superci/<id>/ in SSM Parameter Store, written by the dashboard; a schedule calls the sweep every minute,
//! and the function calls itself for a sweep sooner than that (see `Scheduled`). Machines in this account are
//! started with the function's own role, so AWS needs no OpenID Connect here.
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use futures::executor::block_on;
use serde_json::{json, Value};

use superci_core::aws::{self, Credentials};
use superci_core::io::{self, Clock, Http, Request, Response, Store, Timer};
use superci_core::plane::{Cache, Config, ControlPlane};

mod convert;

fn now_ms() -> u64 { SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_millis() as u64).unwrap_or(0) }

fn agent(redirects: u32) -> ureq::Agent {
    ureq::Agent::config_builder().http_status_as_error(false).max_redirects(redirects).timeout_global(Some(Duration::from_secs(25))).build().into()
}

/// The client: one that follows redirects, and one that gives them back as they are (asked of a request for where
/// GitHub keeps a log: the link is the answer, not what is there).
struct Blocking(ureq::Agent, ureq::Agent);

#[async_trait(?Send)]
impl Http for Blocking {
    async fn send(&self, r: Request) -> io::Result<Response> {
        // (In a test, the clouds' stand-in: see `io::stand_in`.)
        let url = io::stand_in(&r.url);
        let mut builder = ureq::http::Request::builder().method(r.method.as_str()).uri(url.as_str());
        for (k, v) in &r.headers { if url == r.url || !k.eq_ignore_ascii_case("host") { builder = builder.header(k.as_str(), v.as_str()) } }
        let request = builder.body(r.body.clone()).map_err(|e| e.to_string())?;
        let mut response = if r.no_follow { &self.1 } else { &self.0 }.run(request).map_err(|e| format!("{} {}: {e}", r.method, r.url))?;
        let headers = response.headers().iter().filter_map(|(k, v)| Some((k.as_str().to_string(), v.to_str().ok()?.to_string()))).collect();
        Ok(Response { status: response.status().as_u16(), headers, body: response.body_mut().read_to_vec().map_err(|e| e.to_string())? })
    }
}

/// The function's own credentials (from its role, in the environment).
fn own_credentials() -> Credentials {
    let var = |n: &str| std::env::var(n).unwrap_or_default();
    Credentials { access_key_id: var("AWS_ACCESS_KEY_ID"), secret_access_key: var("AWS_SECRET_ACCESS_KEY"), session_token: std::env::var("AWS_SESSION_TOKEN").ok(), expires_at_ms: u64::MAX }
}

/// What DynamoDB says when a call may be made again: too much at once, or a passing fault of its own.
const AGAIN: [&str; 6] = ["ThrottlingException", "ProvisionedThroughputExceededException", "RequestLimitExceeded", "InternalServerError", "ServiceUnavailable", "HTTP 5"];

/// State in one DynamoDB table: the record as text (`v`) under its key (`k`, the sort key) in a partition (`p`), and its
/// end in seconds (`x`, the table's time to live) when it has one. A partition is its kind's, the key up to its first
/// colon (`settings`, `day`); but one partition takes some 3,000 reads and 1,000 writes a second, so each kind every job
/// reads and writes (`SPREAD`) is spread over `BUCKETS` of them by the key's second part (`job#07` for `job:1717`; one
/// job's log marks, `glive:<id>:…`, in one). The layouts before are read where they kept a record until `migrate` has
/// moved what is left, a little at each sweep (then `layout` says so): every record in the one partition `s` (before
/// 0.19.0), then each kind whole in its own.
struct Dynamo<'a> { http: &'a dyn Http, table: String, region: String, layout: std::cell::Cell<Option<u8>> }

/// The kinds read and written for every job and every event, each spread over `BUCKETS` partitions.
const SPREAD: [&str; 9] = ["job", "live", "recent", "runner", "hello", "shape", "glive", "cachem", "active"];
const BUCKETS: u32 = 16;

fn kind(key: &str) -> &str { key.split(':').next().unwrap_or(key) }

/// The partition a key is kept in: its kind's, or for a kind spread, the one its second part falls in (FNV-1a, folded;
/// never to change, as a record is looked for where its key says).
fn part(key: &str) -> String {
    let kind = kind(key);
    if !SPREAD.contains(&kind) { return kind.to_string() }
    let hash = key.split(':').nth(1).unwrap_or_default().bytes().fold(0x811c_9dc5u32, |h, b| (h ^ b as u32).wrapping_mul(0x0100_0193));
    format!("{kind}#{:02}", (hash ^ (hash >> 16)) % BUCKETS)
}

/// The partitions a prefix's records are in: one when the prefix holds its kind's second part whole (`glive:17:`), else
/// each of its kind's.
fn parts(prefix: &str) -> Vec<String> {
    let kind = kind(prefix);
    if !SPREAD.contains(&kind) || prefix.splitn(3, ':').count() == 3 { return vec![part(prefix)] }
    (0..BUCKETS).map(|b| format!("{kind}#{b:02}")).collect()
}

/// Where a layout before this one kept a key's records (`layout` its number, see `LAYOUTS`), the newest first.
fn older(key: &str, layout: u8) -> Vec<String> {
    let mut at = vec![];
    if layout < 2 && SPREAD.contains(&kind(key)) { at.push(kind(key).to_string()) }
    if layout < 1 { at.push("s".into()) }
    at
}

fn key_in(p: &str, k: &str) -> Value { json!({ "p": { "S": p }, "k": { "S": k } }) }

/// Where the layout is said to be (once nothing is left where one before it kept records).
const LAYOUT: &str = "layout";

/// What `layout` says of each, in turn: every record in `s` (it said nothing), each kind in one partition, and this one.
const LAYOUTS: [&str; 3] = ["", "kinds", "spread"];

impl Dynamo<'_> {
    /// One call, asked again when DynamoDB says to (throttled, or a passing error of its own), as AWS's own SDKs do:
    /// up to ten times, each wait twice the last at most (from 50 ms to 5 s), a random part of it. Then what DynamoDB
    /// said: an error, never an answer of nothing (what was not read is not known to be absent).
    async fn call(&self, op: &str, body: Value) -> io::Result<Value> {
        let (mut most_ms, mut tries) = (50u64, 0);
        loop {
            tries += 1;
            if tries == 10 { break }
            match aws::json_call(self.http, "POST", &format!("https://dynamodb.{}.amazonaws.com/", self.region), &self.region, "dynamodb", Some(&format!("DynamoDB_20120810.{op}")),
                "application/x-amz-json-1.0", Some(&body), &own_credentials(), now_ms()).await {
                Err(e) if AGAIN.iter().any(|kind| e.starts_with(kind)) => {
                    let r = superci_core::crypto::random_bytes(2);
                    std::thread::sleep(std::time::Duration::from_millis(u16::from_le_bytes([r[0], r[1]]) as u64 % most_ms + 1));
                    most_ms = (most_ms * 2).min(5000);
                }
                done => return done,
            }
        }
        aws::json_call(self.http, "POST", &format!("https://dynamodb.{}.amazonaws.com/", self.region), &self.region, "dynamodb", Some(&format!("DynamoDB_20120810.{op}")),
            "application/x-amz-json-1.0", Some(&body), &own_credentials(), now_ms()).await
    }
    fn item(&self, k: &str, value: String, until_ms: Option<u64>) -> Value {
        let mut item = json!({ "p": { "S": part(k) }, "k": { "S": k }, "v": { "S": value } });
        if let Some(until) = until_ms { item["x"] = json!({ "N": (until / 1000).to_string() }) }
        item
    }
    /// The layout this table is in (asked once per request), by its number in `LAYOUTS`.
    async fn layout(&self) -> io::Result<u8> {
        if let Some(layout) = self.layout.get() { return Ok(layout) }
        let v = self.call("GetItem", json!({ "TableName": self.table, "Key": key_in(LAYOUT, LAYOUT), "ConsistentRead": true })).await?;
        let said = v["Item"]["v"]["S"].as_str().unwrap_or_default();
        let layout = LAYOUTS.iter().position(|l| *l == said).unwrap_or(0) as u8;
        self.layout.set(Some(layout));
        Ok(layout)
    }
    async fn get_at(&self, p: &str, k: &str, consistent: bool) -> io::Result<Option<String>> {
        let v = self.call("GetItem", json!({ "TableName": self.table, "Key": key_in(p, k), "ConsistentRead": consistent })).await?;
        Ok(v["Item"]["v"]["S"].as_str().map(str::to_string))
    }
    async fn query(&self, partition: &str, prefix: &str, last: Option<usize>, consistent: bool) -> io::Result<Vec<(String, String)>> {
        let (mut out, mut start) = (vec![], Value::Null);
        loop {
            let mut q = json!({ "TableName": self.table, "KeyConditionExpression": "p = :p AND begins_with(k, :k)", "ExpressionAttributeValues": { ":p": { "S": partition }, ":k": { "S": prefix } }, "ConsistentRead": consistent });
            if let Some(n) = last { q["ScanIndexForward"] = false.into(); q["Limit"] = (n - out.len()).into() }
            if !start.is_null() { q["ExclusiveStartKey"] = start }
            let v = self.call("Query", q).await?;
            for item in v["Items"].as_array().into_iter().flatten() {
                if let (Some(k), Some(val)) = (item["k"]["S"].as_str(), item["v"]["S"].as_str()) { out.push((k.to_string(), val.to_string())) }
            }
            start = v["LastEvaluatedKey"].clone();
            if start.is_null() || last.is_some_and(|n| out.len() >= n) { break }
        }
        if last.is_some() { out.reverse() }
        Ok(out)
    }
    /// A conditional write: whether it was made.
    async fn put_when(&self, put: Value) -> io::Result<bool> {
        match self.call("PutItem", put).await { Ok(_) => Ok(true), Err(e) if e.starts_with("ConditionalCheckFailedException") => Ok(false), Err(e) => Err(e) }
    }
    /// Every record under a prefix, by key: where a layout before kept them first, the oldest first, then their own
    /// partitions (one moved between two of these reads is read in its own, and its own copy is the one kept).
    async fn listed(&self, prefix: &str, consistent: bool) -> io::Result<Vec<(String, String)>> {
        let layout = self.layout().await?;
        let mut all = std::collections::BTreeMap::new();
        for p in older(prefix, layout).into_iter().rev().chain(parts(prefix)) { all.extend(self.query(&p, prefix, None, consistent).await?) }
        Ok(all.into_iter().collect())
    }
    /// Records by key, each looked for in its own partition first and then where a layout before kept it.
    async fn read_many(&self, keys: &[String], consistent: bool) -> io::Result<Vec<Option<String>>> {
        let layout = self.layout().await?;
        let mut found: std::collections::HashMap<String, String> = std::collections::HashMap::new();
        let mut want: Vec<String> = keys.iter().cloned().collect::<std::collections::BTreeSet<_>>().into_iter().collect();
        for depth in 0.. {
            let asked: Vec<(String, String)> = want.iter().filter_map(|k| std::iter::once(part(k)).chain(older(k, layout)).nth(depth).map(|p| (p, k.clone()))).collect();
            if asked.is_empty() { break }
            found.extend(self.batch(&asked, consistent).await?);
            want.retain(|k| !found.contains_key(k));
        }
        // (Moved from where a layout before kept it after its own partition was read: there now.)
        if layout < 2 && !want.is_empty() {
            let asked: Vec<(String, String)> = want.iter().map(|k| (part(k), k.clone())).collect();
            found.extend(self.batch(&asked, consistent).await?);
        }
        Ok(keys.iter().map(|k| found.get(k).cloned()).collect())
    }
    /// Records by partition and key, a hundred at a time (DynamoDB's most): what DynamoDB leaves for later is asked
    /// again, and what it still leaves after eight times read one by one. Not found: absent (a read DynamoDB did not
    /// make is asked again or is an error, never taken for one).
    async fn batch(&self, asked: &[(String, String)], consistent: bool) -> io::Result<std::collections::HashMap<String, String>> {
        let mut found = std::collections::HashMap::new();
        for chunk in asked.chunks(100) {
            let mut want: Vec<Value> = chunk.iter().map(|(p, k)| key_in(p, k)).collect();
            let mut tries = 0;
            loop {
                tries += 1;
                let v = self.call("BatchGetItem", json!({ "RequestItems": { &self.table: { "Keys": want, "ConsistentRead": consistent } } })).await?;
                for item in v["Responses"][&self.table].as_array().into_iter().flatten() {
                    if let (Some(k), Some(val)) = (item["k"]["S"].as_str(), item["v"]["S"].as_str()) { found.insert(k.to_string(), val.to_string()); }
                }
                want = v["UnprocessedKeys"][&self.table]["Keys"].as_array().cloned().unwrap_or_default();
                if want.is_empty() || tries == 8 { break }
            }
            for left in &want {
                let (Some(p), Some(k)) = (left["p"]["S"].as_str(), left["k"]["S"].as_str()) else { continue };
                if let Some(val) = self.get_at(p, k, consistent).await? { found.insert(k.to_string(), val); }
            }
        }
        Ok(found)
    }
}

#[async_trait(?Send)]
impl Store for Dynamo<'_> {
    async fn get(&self, key: &str) -> io::Result<Option<String>> {
        let layout = self.layout().await?;
        let older = older(key, layout);
        if let Some(v) = self.get_at(&part(key), key, true).await? { return Ok(Some(v)) }
        for p in &older { if let Some(v) = self.get_at(p, key, true).await? { return Ok(Some(v)) } }
        // (Moved from where a layout before kept it after its own partition was read: there now.)
        if older.is_empty() { return Ok(None) }
        self.get_at(&part(key), key, true).await
    }
    async fn put(&self, key: &str, value: String) -> io::Result<()> {
        self.call("PutItem", json!({ "TableName": self.table, "Item": self.item(key, value, None) })).await.map(|_| ())
    }
    /// Requests run in parallel here: DynamoDB's condition makes the claim atomic.
    async fn put_if_absent(&self, key: &str, value: String) -> io::Result<bool> {
        for p in older(key, self.layout().await?) { if self.get_at(&p, key, true).await?.is_some() { return Ok(false) } }
        self.put_when(json!({ "TableName": self.table, "Item": self.item(key, value, None), "ConditionExpression": "attribute_not_exists(k)" })).await
    }
    async fn swap(&self, key: &str, was: Option<&str>, value: String) -> io::Result<bool> { self.swap_until(key, was, value, None).await }
    async fn delete(&self, key: &str) -> io::Result<()> {
        for p in std::iter::once(part(key)).chain(older(key, self.layout().await?)) { self.call("DeleteItem", json!({ "TableName": self.table, "Key": key_in(&p, key) })).await?; }
        Ok(())
    }
    async fn list(&self, prefix: &str) -> io::Result<Vec<(String, String)>> { self.listed(prefix, true).await }
    /// Eventually consistent: half the price of `list`.
    async fn list_stale(&self, prefix: &str) -> io::Result<Vec<(String, String)>> { self.listed(prefix, false).await }
    async fn put_until(&self, key: &str, value: String, until_ms: u64) -> io::Result<()> {
        self.call("PutItem", json!({ "TableName": self.table, "Item": self.item(key, value, Some(until_ms)) })).await.map(|_| ())
    }
    /// The same for a write over what was read: only while the value is still that one. (A record still where a layout
    /// before kept it is written in its own partition, only while that one is not there yet, and goes from the older.)
    async fn swap_until(&self, key: &str, was: Option<&str>, value: String, until_ms: Option<u64>) -> io::Result<bool> {
        let older = older(key, self.layout().await?);
        let mut from = None;
        if let (Some(was), false) = (was, older.is_empty()) {
            if self.get_at(&part(key), key, true).await?.is_none() {
                for p in &older {
                    if let Some(v) = self.get_at(p, key, true).await? { if v == was { from = Some(p.clone()) } break }
                }
            }
        }
        let mut put = json!({ "TableName": self.table, "Item": self.item(key, value, until_ms) });
        match (was, &from) {
            (Some(was), None) => { put["ConditionExpression"] = "v = :was".into(); put["ExpressionAttributeValues"] = json!({ ":was": { "S": was } }) }
            _ => put["ConditionExpression"] = "attribute_not_exists(k)".into(),
        }
        let made = self.put_when(put).await?;
        if let (true, Some(p)) = (made, from) { self.call("DeleteItem", json!({ "TableName": self.table, "Key": key_in(&p, key) })).await?; }
        Ok(made)
    }
    async fn list_last(&self, prefix: &str, n: usize) -> io::Result<Vec<(String, String)>> {
        if n == 0 { return Ok(vec![]) }
        let mut all = if older(prefix, self.layout().await?).is_empty() {
            let mut each = vec![];
            for p in parts(prefix) { each.extend(self.query(&p, prefix, Some(n), true).await?) }
            each.sort_by(|a, b| a.0.cmp(&b.0));
            each
        } else { self.list(prefix).await? };
        Ok(all.split_off(all.len().saturating_sub(n)))
    }
    async fn get_many(&self, keys: &[String]) -> io::Result<Vec<Option<String>>> { self.read_many(keys, true).await }
    /// Eventually consistent: half the price of `get_many`.
    async fn get_many_stale(&self, keys: &[String]) -> io::Result<Vec<Option<String>>> { self.read_many(keys, false).await }
    /// Moves what is left where a layout before kept it into each record's own partition, with its end (not over one
    /// written there since), the oldest layout first; then says the layout is this one.
    async fn migrate(&self, budget: usize) -> io::Result<bool> {
        let layout = self.layout().await?;
        if layout as usize == LAYOUTS.len() - 1 { return Ok(true) }
        let from = (if layout < 1 { vec!["s".to_string()] } else { vec![] }).into_iter().chain(SPREAD.iter().map(|k| k.to_string()));
        for p in from {
            let v = self.call("Query", json!({ "TableName": self.table, "KeyConditionExpression": "p = :p", "ExpressionAttributeValues": { ":p": { "S": p } }, "Limit": budget.max(1), "ConsistentRead": true })).await?;
            let items = v["Items"].as_array().cloned().unwrap_or_default();
            for item in &items {
                let (Some(k), true) = (item["k"]["S"].as_str(), item["v"]["S"].is_string()) else { continue };
                let mut moved = item.clone();
                moved["p"] = json!({ "S": part(k) });
                self.put_when(json!({ "TableName": self.table, "Item": moved, "ConditionExpression": "attribute_not_exists(k)" })).await?;
                self.call("DeleteItem", json!({ "TableName": self.table, "Key": key_in(&p, k) })).await?;
            }
            if !items.is_empty() || !v["LastEvaluatedKey"].is_null() { return Ok(false) }
        }
        self.put(LAYOUT, LAYOUTS[LAYOUTS.len() - 1].into()).await?;
        self.layout.set(Some((LAYOUTS.len() - 1) as u8));
        Ok(true)
    }
}

struct SystemClock;
impl Clock for SystemClock { fn now_ms(&self) -> u64 { now_ms() } }

/// A wake-up asked for within this long is the function's own doing; a later one is left to the schedule.
const SOON_MS: u64 = 30_000;

/// The schedule calls the sweep every minute, which is late for what is asked for sooner: a start that failed and
/// is tried again, jobs waiting for room (twenty seconds). For those the function calls itself, not waiting for the
/// answer; that call waits out the time and sweeps (see `main`). One such call is on its way at a time, however
/// many jobs ask (claimed in the table): a sweep is for every job, and asks for the next itself while any waits.
/// Where it may not call itself (a role from before), the schedule does it, as it did.
struct Scheduled<'a> { http: &'a dyn Http, store: &'a Dynamo<'a>, region: &'a str, function: &'a str,
    /// What is to be done once the request at hand is answered (see `main`).
    later: &'a std::cell::RefCell<Vec<String>> }

/// Where a sweep that is going notes itself (when it began, ms).
const SWEEPING: &str = "aws:sweeping";

/// Where a sweep that is on its way is noted (when it is for, ms). One still there well after its time never came.
const WAKING: &str = "aws:waking";

/// Claims the sweep for this request (see `SWEEPING`): the claim, to give back once done, or none while another sweep is
/// going. A claim older than a minute and a half is of a sweep that was cut off, taken over by one alone. (The claim not
/// read, as when DynamoDB is throttled: an error, and no sweep. Two at once would each act on the jobs.)
fn sweep_claim(store: &Dynamo, now: u64) -> io::Result<Option<String>> {
    let token = now.to_string();
    if !block_on(store.put_if_absent(SWEEPING, token.clone()))? {
        let held = block_on(store.get(SWEEPING))?;
        let since = held.as_deref().and_then(|v| v.parse::<u64>().ok()).unwrap_or(0);
        if now.saturating_sub(since) < 90_000 || !block_on(store.swap(SWEEPING, held.as_deref(), token.clone()))? { return Ok(None) }
    }
    Ok(Some(token))
}

/// A sweep's claim given back once it is done.
fn sweep_done(store: &Dynamo, token: &str) {
    let _ = token;
    let _ = block_on(store.delete(SWEEPING));
}

#[async_trait(?Send)]
impl Timer for Scheduled<'_> {
    async fn wake_in(&self, ms: u64) -> io::Result<()> {
        if ms > SOON_MS || self.function.is_empty() { return Ok(()) }
        let at = now_ms() + ms;
        // (Nothing here fails what asked: the schedule's sweep comes within the minute whatever happens.)
        if !self.store.put_if_absent(WAKING, at.to_string()).await.unwrap_or(false) {
            let coming = self.store.get(WAKING).await.ok().flatten().and_then(|v| v.parse::<u64>().ok()).unwrap_or(0);
            if now_ms() < coming + SOON_MS { return Ok(()) }
            let _ = self.store.put(WAKING, at.to_string()).await;
        }
        let body = json!({ "superci": "wake", "at": at }).to_string();
        let url = format!("https://lambda.{}.amazonaws.com/2015-03-31/functions/{}/invocations", self.region, self.function);
        let headers = vec![("content-type".to_string(), "application/json".to_string()), ("x-amz-invocation-type".to_string(), "Event".to_string())];
        let sent = match aws::sigv4("POST", &url, &headers, body.as_bytes(), &own_credentials(), self.region, "lambda", &aws::amz_date(now_ms())) {
            Ok(signed) => { let mut r = Request::new("POST", &url).with_body(body); r.headers = signed; self.http.send(r).await.is_ok_and(|r| r.status < 300) }
            Err(_) => false,
        };
        if !sent { let _ = self.store.delete(WAKING).await; }
        Ok(())
    }
    /// A Lambda answers one request at a time: it simply waits.
    async fn pause(&self, ms: u64) { std::thread::sleep(Duration::from_millis(ms)) }
    /// Done by `main` once the answer is given: the function goes on running until it asks for its next request.
    fn later(&self, what: &str) -> bool { self.later.borrow_mut().push(what.to_string()); true }
}

/// The secrets under /superci/<id>/, read at most every 15 seconds.
struct Secrets { at: Option<Instant>, values: Vec<(String, String)> }

impl Secrets {
    /// `fresh`: a dashboard key not seen yet arrived (a new session just set it): read again now, at most every second.
    fn refresh(&mut self, http: &Blocking, region: &str, plane_id: &str, fresh: bool) -> io::Result<()> {
        let within = if fresh { Duration::from_secs(1) } else { Duration::from_secs(15) };
        if self.at.is_some_and(|t| t.elapsed() < within) { return Ok(()) }
        let (mut values, mut next) = (vec![], None::<String>);
        loop {
            let mut body = json!({ "Path": format!("/superci/{plane_id}/"), "Recursive": false, "WithDecryption": true, "MaxResults": 10 });
            if let Some(n) = &next { body["NextToken"] = json!(n) }
            let v = block_on(aws::json_call(http, "POST", &format!("https://ssm.{region}.amazonaws.com/"), region, "ssm", Some("AmazonSSM.GetParametersByPath"), "application/x-amz-json-1.1", Some(&body), &own_credentials(), now_ms()))?;
            for p in v["Parameters"].as_array().into_iter().flatten() {
                if let (Some(n), Some(val)) = (p["Name"].as_str(), p["Value"].as_str()) { values.push((n.rsplit('/').next().unwrap_or(n).to_string(), val.to_string())) }
            }
            next = v["NextToken"].as_str().map(str::to_string);
            if next.is_none() { break }
        }
        self.values = values;
        self.at = Some(Instant::now());
        Ok(())
    }
}

fn config(secrets: &Secrets, account_id: &str) -> Config {
    let var = |n: &str| std::env::var(n).ok().filter(|v| !v.is_empty());
    let mut c = Config::new(var("PLANE_ID").unwrap_or_default());
    if let Some(v) = var("LABEL") { c.label = v.to_ascii_lowercase() }
    if let Some(v) = var("INSTANCE_TYPES") { c.instance_types = v.split(',').map(|s| s.trim().to_string()).filter(|s| !s.is_empty()).collect() }
    c.read_settings(&secrets.values);
    c.aws_own = Some((account_id.to_string(), var("RUNNER_REGION").or(var("AWS_REGION")).unwrap_or_default()));
    c.aws_own_creds = Some(own_credentials());
    c
}

/// A function URL event (payload 2.0) as a request.
fn request(event: &Value) -> Request {
    let ctx = &event["requestContext"];
    let query = event["rawQueryString"].as_str().filter(|q| !q.is_empty()).map(|q| format!("?{q}")).unwrap_or_default();
    let url = format!("https://{}{}{query}", ctx["domainName"].as_str().unwrap_or_default(), event["rawPath"].as_str().unwrap_or("/"));
    let body = event["body"].as_str().unwrap_or_default();
    let body = if event["isBase64Encoded"] == true { STANDARD.decode(body).unwrap_or_default() } else { body.as_bytes().to_vec() };
    let mut r = Request::new(ctx["http"]["method"].as_str().unwrap_or("GET"), &url).with_body(body);
    for (k, v) in event["headers"].as_object().into_iter().flatten() { if let Some(v) = v.as_str() { r.headers.push((k.to_ascii_lowercase(), v.to_string())) } }
    r
}

fn response(r: Response) -> Value {
    let headers: serde_json::Map<String, Value> = r.headers.into_iter().map(|(k, v)| (k, Value::String(v))).collect();
    json!({ "statusCode": r.status, "headers": headers, "body": STANDARD.encode(&r.body), "isBase64Encoded": true })
}

/// The converter's loop (see `convert`): one layer a request, answered with what was made.
fn converter(api: &str) -> ! {
    let (http, runtime) = (Blocking(agent(10), agent(0)), ureq::Agent::from(ureq::Agent::config_builder().http_status_as_error(false).build()));
    let mkfs = format!("{}/mkfs.erofs", std::env::var("LAMBDA_TASK_ROOT").unwrap_or_else(|_| "/var/task".into()));
    loop {
        let Ok(mut next) = runtime.get(&format!("http://{api}/2018-06-01/runtime/invocation/next")).call() else { continue };
        let id = next.headers().get("lambda-runtime-aws-request-id").and_then(|v| v.to_str().ok()).unwrap_or_default().to_string();
        let event: Value = next.body_mut().read_json().unwrap_or_default();
        let done = match serde_json::from_value::<convert::Job>(event) {
            Ok(job) => { let done = convert::convert(&http, &own_credentials(), &job, &mkfs, &std::env::temp_dir()); eprintln!("superci: {done}"); done }
            Err(e) => json!({ "error": format!("not a layer to convert: {e}") }),
        };
        let _ = runtime.post(&format!("http://{api}/2018-06-01/runtime/invocation/{id}/response")).send_json(done);
    }
}

fn main() {
    let api = std::env::var("AWS_LAMBDA_RUNTIME_API").expect("runs in AWS Lambda");
    if std::env::var("SUPERCI_CONVERTER").is_ok_and(|v| v == "1") { converter(&api) }
    let region = std::env::var("AWS_REGION").unwrap_or_default();
    let (http, runtime) = (Blocking(agent(10), agent(0)), ureq::Agent::from(ureq::Agent::config_builder().http_status_as_error(false).build()));
    let table = std::env::var("TABLE").unwrap_or_default();
    let plane_id = std::env::var("PLANE_ID").unwrap_or_default();
    let function = std::env::var("AWS_LAMBDA_FUNCTION_NAME").unwrap_or_default();
    let cache = Cache::default();
    let mut secrets = Secrets { at: None, values: vec![] };
    loop {
        let Ok(mut next) = runtime.get(&format!("http://{api}/2018-06-01/runtime/invocation/next")).call() else { continue };
        let id = next.headers().get("lambda-runtime-aws-request-id").and_then(|v| v.to_str().ok()).unwrap_or_default().to_string();
        let account_id = next.headers().get("lambda-runtime-invoked-function-arn").and_then(|v| v.to_str().ok()).and_then(|a| a.split(':').nth(4)).unwrap_or_default().to_string();
        let event: Value = next.body_mut().read_json().unwrap_or_default();
        // Whoever comes with a key (the dashboard, a command) gets the settings as they are now, not as they
        // were up to fifteen seconds ago: a change is seen at once, and the next one starts from it. A key not
        // known yet is a new session's, set a moment ago. Webhooks and the sweep carry none, and use what was read.
        let bearer = event["headers"]["authorization"].as_str().and_then(|a| a.strip_prefix("Bearer ")).unwrap_or_default();
        let unknown = bearer.len() >= 16;
        // A move's steps need what the dashboard wrote a moment ago (the App, GitLab): read fresh for them too.
        let moving = event["rawPath"].as_str().is_some_and(|p| p.starts_with("/move/"));
        let refreshed = secrets.refresh(&http, &region, &plane_id, unknown || moving);
        let config = config(&secrets, &account_id);
        let store = Dynamo { http: &http, table: table.clone(), region: region.clone(), layout: std::cell::Cell::new(None) };
        let later = std::cell::RefCell::new(vec![]);
        let timer = Scheduled { http: &http, store: &store, region: &region, function: &function, later: &later };
        let plane = ControlPlane { store: &store, http: &http, clock: &SystemClock, timer: &timer, config: &config, cache: &cache, containers: None };
        let outcome: io::Result<Value> = (|| {
            refreshed?;
            // One sweep at a time: the schedule's and the function's own could otherwise each start a machine for
            // the same waiting job. One that finds another going leaves it to that one (which asks for the next
            // itself); a note older than a minute and a half is of a sweep that was cut off, taken over by one alone.
            // (The note not read, as when DynamoDB is throttled: no sweep. Two at once would each act on the jobs.)
            let sweep = || -> io::Result<Value> {
                let Some(token) = sweep_claim(&store, now_ms())? else { return Ok(json!({ "swept": false })) };
                let done = block_on(plane.alarm()).map(|_| json!({ "swept": true }));
                sweep_done(&store, &token);
                done
            };
            if event["superci"] == "sweep" { return sweep() }
            // Its own call for a sweep sooner than the schedule's: the time is waited out here, then the sweep.
            if event["superci"] == "wake" {
                let at = event["at"].as_u64().unwrap_or(0);
                std::thread::sleep(Duration::from_millis(at.saturating_sub(now_ms()).min(SOON_MS)));
                let _ = block_on(store.delete(WAKING));
                return sweep()
            }
            Ok(response(block_on(plane.handle(request(&event)))))
        })();
        let _ = match outcome {
            Ok(v) => runtime.post(&format!("http://{api}/2018-06-01/runtime/invocation/{id}/response")).send_json(v),
            Err(e) => { eprintln!("superci: {e}"); runtime.post(&format!("http://{api}/2018-06-01/runtime/invocation/{id}/error")).send_json(json!({ "errorMessage": e, "errorType": "superci" })) }
        };
        // What was left for after the answer (a queued job's machine: GitHub has its answer, and AWS is asked now).
        // The function runs on until it asks for its next request, and takes no other meanwhile.
        let url = request(&event).url;
        // (One thing done may leave another: each in its turn, until none is left.)
        loop {
            let next = { let mut left = later.borrow_mut(); if left.is_empty() { None } else { Some(left.remove(0)) } };
            let Some(what) = next else { break };
            if let Err(e) = block_on(plane.later(&url, &what)) { eprintln!("superci: {what}: {e}") }
        }
    }
}

#[cfg(test)]
mod tests {
    //! The DynamoDB store against a stand-in for DynamoDB's own API (its calls and answers as AWS gives them, kept in
    //! memory): what every store promises, the partitions it keeps records in, ends, and moving the layout before
    //! 0.19.0 over while it is read.
    use super::*;
    use std::cell::RefCell;
    use std::collections::BTreeMap;

    /// DynamoDB in memory: items by (partition, key), with the calls the store makes. It answers a page of two items at
    /// a time (so the store pages), leaves some keys of a batch for later the first time, and says it is throttled
    /// once in a while (so the store asks again).
    #[derive(Default)]
    struct FakeDynamo { items: RefCell<BTreeMap<(String, String), Value>>, calls: RefCell<u64>, unprocessed_once: RefCell<bool>,
        /// The call (counted from 1) answered with an error the store does not ask again for.
        fail_at: RefCell<Option<u64>>,
        /// A query's next page said to be empty, more after it (as DynamoDB may answer).
        empty_page_once: RefCell<bool>,
        /// Throttled ever more (every call): what asking again does not get past.
        throttled: RefCell<bool>,
        /// Every batch leaves all but its first key for later.
        unprocessed_always: RefCell<bool>,
        /// No call throttled now and then; queries answered a thousand items a page (as DynamoDB, up to a megabyte).
        calm: RefCell<bool>,
        /// Calls of these kinds throttled every time.
        throttle_ops: RefCell<Vec<&'static str>>,
        /// What the calls consumed, as DynamoDB counts it, by partition: read units (4 KB, half for an eventually
        /// consistent read) and write units (1 KB).
        units: RefCell<BTreeMap<String, (f64, f64)>>,
        /// What another request does just before a call is made (its kind, what it asks, the items), as a test sets it.
        meddle: RefCell<Option<Meddler>>,
        /// Each call waits its turn among the requests going at once (see `Turn`), as the calls of requests that run at
        /// once interleave.
        interleaved: RefCell<bool>,
        /// How long the calls took, one after another (µs, see `LATENCY_US`); how many of each kind; the conditional
        /// writes refused, by partition.
        took_us: RefCell<u64>,
        ops: RefCell<BTreeMap<String, u64>>,
        lost: RefCell<BTreeMap<String, u64>> }

    type Meddler = Box<dyn FnMut(&str, &Value, &mut BTreeMap<(String, String), Value>)>;

    /// How long each of DynamoDB's calls takes from a function in its region (µs): an assumption, of the order AWS says
    /// for single-digit-millisecond reads and writes, not a measurement.
    const LATENCY_US: [(&str, u64); 5] = [("GetItem", 4_000), ("PutItem", 6_000), ("DeleteItem", 6_000), ("Query", 8_000), ("BatchGetItem", 12_000)];

    /// How long each call to GitHub or to CloudWatch takes (µs): likewise an assumption.
    const HTTP_US: u64 = 60_000;

    /// A call's turn: once every other request going has had one (it is woken again at once, behind them).
    struct Turn(bool);
    impl std::future::Future for Turn {
        type Output = ();
        fn poll(mut self: std::pin::Pin<&mut Self>, cx: &mut std::task::Context<'_>) -> std::task::Poll<()> {
            if self.0 { return std::task::Poll::Ready(()) }
            self.0 = true;
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    }

    /// An item's size as DynamoDB counts it: its attributes' names and values.
    fn size(item: &Value) -> usize { item.as_object().map_or(0, |o| o.iter().map(|(k, v)| k.len() + v.as_object().and_then(|t| t.values().next()).and_then(Value::as_str).map_or(0, str::len)).sum()) }

    #[async_trait(?Send)]
    impl Http for FakeDynamo {
        async fn send(&self, r: Request) -> io::Result<Response> {
            if *self.interleaved.borrow() { Turn(false).await }
            let n = { let mut c = self.calls.borrow_mut(); *c += 1; *c };
            let op = r.header("x-amz-target").unwrap_or_default().trim_start_matches("DynamoDB_20120810.").to_string();
            *self.ops.borrow_mut().entry(op.clone()).or_default() += 1;
            *self.took_us.borrow_mut() += LATENCY_US.iter().find(|(o, _)| *o == op).map_or(0, |(_, us)| *us);
            let fail = |kind: &str| Ok(Response::new(400, "application/x-amz-json-1.0", json!({ "__type": format!("com.amazonaws.dynamodb.v20120810#{kind}"), "message": kind }).to_string()));
            if (n % 13 == 0 && !*self.calm.borrow()) || *self.throttled.borrow() || self.throttle_ops.borrow().contains(&op.as_str()) { return fail("ThrottlingException") }
            if *self.fail_at.borrow() == Some(n) { return fail("ValidationException") }
            let b: Value = serde_json::from_slice(&r.body).unwrap_or_default();
            if let Some(meddle) = self.meddle.borrow_mut().as_mut() { meddle(&op, &b, &mut self.items.borrow_mut()) }
            let key = |k: &Value| (k["p"]["S"].as_str().unwrap_or_default().to_string(), k["k"]["S"].as_str().unwrap_or_default().to_string());
            let ok = |v: Value| Ok(Response::new(200, "application/x-amz-json-1.0", v.to_string()));
            let mut items = self.items.borrow_mut();
            let factor = if b["ConsistentRead"] == true { 1.0 } else { 0.5 };
            let read = |p: &str, bytes: usize| self.units.borrow_mut().entry(p.to_string()).or_default().0 += (bytes.div_ceil(4096).max(1)) as f64 * factor;
            let wrote = |p: &str, bytes: usize| self.units.borrow_mut().entry(p.to_string()).or_default().1 += bytes.div_ceil(1024).max(1) as f64;
            match op.as_str() {
                "GetItem" => { let at = key(&b["Key"]); read(&at.0, items.get(&at).map_or(0, size)); ok(match items.get(&at) { Some(i) => json!({ "Item": i }), None => json!({}) }) }
                "PutItem" => {
                    let at = key(&b["Item"]);
                    wrote(&at.0, size(&b["Item"]).max(items.get(&at).map_or(0, size)));
                    let held = items.get(&at);
                    let allowed = match b["ConditionExpression"].as_str() {
                        Some("attribute_not_exists(k)") => held.is_none(),
                        Some("v = :was") => held.is_some_and(|h| h["v"] == b["ExpressionAttributeValues"][":was"]),
                        _ => true,
                    };
                    if !allowed { *self.lost.borrow_mut().entry(at.0.clone()).or_default() += 1; return fail("ConditionalCheckFailedException") }
                    items.insert(at, b["Item"].clone());
                    ok(json!({}))
                }
                "DeleteItem" => {
                    let at = key(&b["Key"]);
                    wrote(&at.0, items.get(&at).map_or(0, size));
                    if b["ConditionExpression"] == "v = :was" && items.get(&at).is_none_or(|h| h["v"] != b["ExpressionAttributeValues"][":was"]) { return fail("ConditionalCheckFailedException") }
                    items.remove(&at);
                    ok(json!({}))
                }
                "Query" => {
                    let vals = &b["ExpressionAttributeValues"];
                    let (p, prefix) = (vals[":p"]["S"].as_str().unwrap_or_default(), vals[":k"]["S"].as_str().unwrap_or(""));
                    let mut hits: Vec<Value> = items.iter().filter(|((ip, ik), _)| ip == p && ik.starts_with(prefix)).map(|(_, i)| i.clone()).collect();
                    if b["ScanIndexForward"] == false { hits.reverse() }
                    let after = b["ExclusiveStartKey"]["k"]["S"].as_str().map(str::to_string);
                    let forward = b["ScanIndexForward"] != false;
                    let start = after.map(|a| hits.iter().position(|i| i["k"]["S"].as_str() == Some(a.as_str())).map(|x| x + 1).unwrap_or(0)).unwrap_or(0);
                    if self.empty_page_once.replace(false) { return ok(json!({ "Items": [], "LastEvaluatedKey": { "p": { "S": p }, "k": { "S": "" } } })) }
                    let most = if *self.calm.borrow() { 1_000 } else { 2 };
                    let page = b["Limit"].as_u64().map_or(most, |l| (l as usize).min(most));
                    let shown: Vec<Value> = hits.iter().skip(start).take(page).cloned().collect();
                    read(p, shown.iter().map(size).sum());
                    let more = start + page < hits.len();
                    let _ = forward;
                    ok(if more { json!({ "Items": shown, "LastEvaluatedKey": { "p": { "S": p }, "k": shown.last().unwrap()["k"] } }) } else { json!({ "Items": shown }) })
                }
                "BatchGetItem" => {
                    let table = b["RequestItems"].as_object().unwrap().keys().next().unwrap().clone();
                    let keys = b["RequestItems"][&table]["Keys"].as_array().cloned().unwrap_or_default();
                    let first = !*self.unprocessed_once.borrow();
                    *self.unprocessed_once.borrow_mut() = true;
                    let (now, later) = if (first || *self.unprocessed_always.borrow()) && keys.len() > 1 { keys.split_at(1) } else { keys.split_at(keys.len()) };
                    for k in now { let at = key(k); read(&at.0, items.get(&at).map_or(0, size)) }
                    let found: Vec<Value> = now.iter().filter_map(|k| items.get(&key(k)).cloned()).collect();
                    ok(json!({ "Responses": { &table: found }, "UnprocessedKeys": if later.is_empty() { json!({}) } else { json!({ &table: { "Keys": later } }) } }))
                }
                other => panic!("a call the store does not make: {other}"),
            }
        }
    }

    fn store(fake: &FakeDynamo) -> Dynamo<'_> { Dynamo { http: fake, table: "superci-plane-t".into(), region: "us-east-1".into(), layout: std::cell::Cell::new(None) } }

    /// Lambda taking (or refusing) the function's calls to itself; DynamoDB as above.
    #[derive(Default)]
    struct Lambda { refuse: std::cell::Cell<bool>, called: RefCell<Vec<Request>> }
    #[async_trait(?Send)]
    impl Http for Lambda {
        async fn send(&self, r: Request) -> io::Result<Response> {
            self.called.borrow_mut().push(r);
            Ok(Response::new(if self.refuse.get() { 403 } else { 202 }, "application/json", "{}"))
        }
    }

    #[test]
    fn a_sweep_sooner_than_the_schedules_is_one_call_to_itself_at_a_time() {
        let fake = FakeDynamo::default();
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "kinds" } }));
        let s = store(&fake);
        let (lambda, later) = (Lambda::default(), RefCell::new(vec![]));
        let timer = Scheduled { http: &lambda, store: &s, region: "us-east-1", function: "superci-plane-t", later: &later };
        // Later than half a minute: the schedule's minute does it.
        block_on(timer.wake_in(60_000)).unwrap();
        assert!(lambda.called.borrow().is_empty());
        // Sooner: one call, not waited for, signed, saying when.
        block_on(timer.wake_in(20_000)).unwrap();
        let called = lambda.called.borrow()[0].clone();
        assert_eq!((called.url.as_str(), called.header("x-amz-invocation-type")), ("https://lambda.us-east-1.amazonaws.com/2015-03-31/functions/superci-plane-t/invocations", Some("Event")));
        assert!(called.header("authorization").is_some_and(|a| a.contains("/us-east-1/lambda/aws4_request")));
        let at = serde_json::from_slice::<Value>(&called.body).unwrap()["at"].as_u64().unwrap();
        assert!(at >= now_ms() + 19_000 && at <= now_ms() + 20_000);
        // Another asked for meanwhile: the one on its way does for both.
        block_on(timer.wake_in(1_000)).unwrap();
        assert_eq!(lambda.called.borrow().len(), 1);
        // One noted long ago never came: a call again.
        block_on(s.put(WAKING, (now_ms() - 120_000).to_string())).unwrap();
        block_on(timer.wake_in(1_000)).unwrap();
        assert_eq!(lambda.called.borrow().len(), 2);
        // Refused (a role from before, which may not call itself): its note taken away, the schedule does it.
        block_on(s.delete(WAKING)).unwrap();
        lambda.refuse.set(true);
        block_on(timer.wake_in(1_000)).unwrap();
        assert_eq!(block_on(s.get(WAKING)).unwrap(), None);
        // A function with no name (not on Lambda) calls nothing; what is left for later is kept for after the answer.
        let none = Scheduled { http: &lambda, store: &s, region: "us-east-1", function: "", later: &later };
        block_on(none.wake_in(1_000)).unwrap();
        assert_eq!(lambda.called.borrow().len(), 3);
        assert!(timer.later("start job:1"));
        assert_eq!(*later.borrow(), ["start job:1"]);
        let began = Instant::now();
        block_on(timer.pause(30));
        assert!(began.elapsed() >= Duration::from_millis(30));
    }

    #[test]
    fn it_keeps_each_kind_of_record_in_a_partition_of_its_own_and_does_what_every_store_does() {
        let fake = FakeDynamo::default();
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "spread" } }));
        let s = store(&fake);
        block_on(s.put("job:1", "one".into())).unwrap();
        block_on(s.put("job:2", "two".into())).unwrap();
        block_on(s.put("job:3", "three".into())).unwrap();
        block_on(s.put("live:2", String::new())).unwrap();
        block_on(s.put("settings", "s".into())).unwrap();
        let held = |p: &str, k: &str| fake.items.borrow().contains_key(&(p.to_string(), k.to_string()));
        assert!(held(&part("job:1"), "job:1") && held(&part("live:2"), "live:2") && held("settings", "settings"));
        assert!(part("job:1").starts_with("job#") && part("glive:9:a:0001") == part("glive:9:b:0002") && parts("glive:9:") == [part("glive:9:x")] && parts("job:").len() == BUCKETS as usize);
        assert_eq!(block_on(s.get("job:2")).unwrap().as_deref(), Some("two"));
        assert_eq!(block_on(s.list("job:")).unwrap().len(), 3, "every page of it");
        assert_eq!(block_on(s.list_last("job:", 2)).unwrap().iter().map(|(k, _)| k.as_str()).collect::<Vec<_>>(), ["job:2", "job:3"]);
        assert_eq!(block_on(s.get_many(&["job:3".into(), "job:9".into(), "job:1".into()])).unwrap(), [Some("three".to_string()), None, Some("one".to_string())], "what a batch left for later is asked again");
        assert!(!block_on(s.put_if_absent("job:1", "again".into())).unwrap() && block_on(s.put_if_absent("job:4", "four".into())).unwrap());
        assert!(!block_on(s.swap("job:1", Some("not this"), "x".into())).unwrap() && block_on(s.swap("job:1", Some("one"), "uno".into())).unwrap());
        block_on(s.put_until("rline:t:1", "lines".into(), 1_790_000_000_000)).unwrap();
        assert_eq!(fake.items.borrow()[&("rline".to_string(), "rline:t:1".to_string())]["x"]["N"], "1790000000", "its end in seconds, for the table's time to live");
        block_on(s.delete("job:4")).unwrap();
        assert_eq!(block_on(s.get("job:4")).unwrap(), None);
        assert!(block_on(s.migrate(10)).unwrap(), "nothing of an earlier layout");
    }

    #[test]
    fn records_of_the_layout_before_are_read_where_they_are_and_moved_over_a_little_at_a_time() {
        let fake = FakeDynamo::default();
        let old = |k: &str, v: &str| (("s".to_string(), k.to_string()), json!({ "p": { "S": "s" }, "k": { "S": k }, "v": { "S": v } }));
        fake.items.borrow_mut().extend([old("job:1", "one"), old("job:2", "two"), old("job:3", "three"), old("hello:1", "h")]);
        let s = store(&fake);
        // Read as they are, before anything moved; a write over one goes to its own partition.
        assert_eq!(block_on(s.get("job:2")).unwrap().as_deref(), Some("two"));
        assert_eq!(block_on(s.list("job:")).unwrap().len(), 3);
        assert!(!block_on(s.put_if_absent("job:1", "x".into())).unwrap(), "one in the old partition is there");
        assert!(block_on(s.swap("job:3", Some("three"), "tres".into())).unwrap(), "written in its own partition, the old one gone");
        assert!(fake.items.borrow().contains_key(&(part("job:3"), "job:3".to_string())) && !fake.items.borrow().contains_key(&("s".to_string(), "job:3".to_string())));
        // Moved a few at each sweep, never over what was written since; then the layout says so.
        assert!(!block_on(s.migrate(2)).unwrap());
        while !block_on(s.migrate(2)).unwrap() {}
        assert!(!fake.items.borrow().keys().any(|(p, _)| p == "s"), "nothing left in the old partition");
        assert_eq!(block_on(s.get("job:3")).unwrap().as_deref(), Some("tres"));
        assert_eq!(block_on(s.get("hello:1")).unwrap().as_deref(), Some("h"));
        assert_eq!(fake.items.borrow()[&("layout".to_string(), "layout".to_string())]["v"]["S"], "spread");
    }

    /// A table as 0.20.5 left it, each kind whole in one partition: read where its records are, written in their own
    /// partitions (one written over is moved), and moved over a little at each sweep with their ends, never over what was
    /// written since; then the layout says so, and what every job reads and writes is spread over many partitions.
    #[test]
    fn a_table_with_each_kind_in_one_partition_is_spread_while_it_is_used() {
        let fake = FakeDynamo::default();
        let at = |p: &str, k: &str, v: &str| ((p.to_string(), k.to_string()), json!({ "p": { "S": p }, "k": { "S": k }, "v": { "S": v }, "x": { "N": "1790000000" } }));
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "kinds" } }));
        fake.items.borrow_mut().extend((0..40).map(|i| at("job", &format!("job:{i}"), "kept")));
        fake.items.borrow_mut().extend([at("live", "live:3", ""), at("glive", "glive:3:a:0001", "m1"), at("glive", "glive:3:a:0002", "m2"), at("settings", "settings", "s")]);
        let s = store(&fake);
        assert_eq!(block_on(s.get("job:7")).unwrap().as_deref(), Some("kept"));
        assert_eq!(block_on(s.get("job:99")).unwrap(), None);
        assert_eq!(block_on(s.get_many(&["job:7".into(), "job:99".into(), "live:3".into()])).unwrap(), [Some("kept".to_string()), None, Some(String::new())]);
        assert_eq!(block_on(s.list_stale("job:")).unwrap().len(), 40);
        assert_eq!(block_on(s.list("glive:3:")).unwrap().len(), 2);
        block_on(s.put("job:100", "new".into())).unwrap();
        assert!(block_on(s.swap("job:8", Some("kept"), "swapped".into())).unwrap());
        assert!(!block_on(s.swap("job:9", Some("not what is kept"), "x".into())).unwrap());
        assert!(!block_on(s.put_if_absent("job:9", "again".into())).unwrap());
        let held = |p: &str, k: &str| fake.items.borrow().contains_key(&(p.to_string(), k.to_string()));
        assert!(held(&part("job:100"), "job:100") && held(&part("job:8"), "job:8") && !held("job", "job:8"));
        assert_eq!(block_on(s.list("job:")).unwrap().len(), 41);
        assert!(!block_on(s.migrate(10)).unwrap());
        while !block_on(s.migrate(10)).unwrap() {}
        {
            let items = fake.items.borrow();
            assert!(items.keys().all(|(p, _)| !SPREAD.contains(&p.as_str())), "nothing left where a kind was kept whole");
            assert_eq!(items[&(part("job:8"), "job:8".to_string())]["v"]["S"], "swapped", "not moved over what was written since");
            assert_eq!(items[&(part("glive:3:a:0001"), "glive:3:a:0001".to_string())]["x"]["N"], "1790000000", "moved with its end");
            assert!(items.contains_key(&("settings".to_string(), "settings".to_string())), "a kind not spread stays");
            let jobs: std::collections::BTreeSet<&String> = items.keys().filter(|(p, _)| p.starts_with("job#")).map(|(p, _)| p).collect();
            assert!(jobs.len() >= 8, "forty jobs over many partitions: {jobs:?}");
            assert_eq!(items[&("layout".to_string(), "layout".to_string())]["v"]["S"], "spread");
        }
        let s = store(&fake);
        assert_eq!(block_on(s.list("job:")).unwrap().len(), 41);
        assert_eq!(block_on(s.list_last("job:", 3)).unwrap().iter().map(|(k, _)| k.as_str()).collect::<Vec<_>>(), ["job:7", "job:8", "job:9"]);
        assert_eq!(block_on(s.get("job:100")).unwrap().as_deref(), Some("new"));
    }

    /// What every store's caller does with the old layout and the new, as one scenario: each step's own error kept,
    /// none stopping the next (what a sweep run of it fails along the way).
    fn every_operation(fake: &FakeDynamo) -> Vec<String> {
        let old = |k: &str, v: &str| (("s".to_string(), k.to_string()), json!({ "p": { "S": "s" }, "k": { "S": k }, "v": { "S": v } }));
        fake.items.borrow_mut().extend([old("job:1", "one"), old("job:2", "two"), old("job:3", "three"), old("hello:1", "h")]);
        fake.items.borrow_mut().insert(("job".into(), "job:5".into()), json!({ "p": { "S": "job" }, "k": { "S": "job:5" }, "v": { "S": "five" } }));
        // (A store each time, as each request has: it asks again where the layout is.)
        let s = || store(fake);
        let mut errors = vec![];
        let mut keep = |r: io::Result<()>| if let Err(e) = r { errors.push(e) };
        keep(block_on(s().get("job:2")).map(|_| ()));
        keep(block_on(s().get("job:9")).map(|_| ()));
        keep(block_on(s().list("job:")).map(|_| ()));
        keep(block_on(s().list_last("job:", 2)).map(|_| ()));
        keep(block_on(s().get_many(&["job:3".into(), "job:9".into(), "job:5".into()])).map(|_| ()));
        keep(block_on(s().get_many(&(0..10).map(|i| format!("job:{i}")).collect::<Vec<_>>())).map(|_| ()));
        keep(block_on(s().put_if_absent("job:1", "x".into())).map(|_| ()));
        keep(block_on(s().put_if_absent("job:6", "six".into())).map(|_| ()));
        keep(block_on(s().swap("job:3", Some("three"), "tres".into())).map(|_| ()));
        keep(block_on(s().swap("job:5", Some("five"), "cinco".into())).map(|_| ()));
        keep(block_on(s().swap("job:7", None, "seven".into())).map(|_| ()));
        // (Not in its own partition, and the old one's value is not the one read: not written.)
        keep(block_on(s().swap("hello:1", Some("not h"), "x".into())).map(|_| ()));
        keep(block_on(s().delete("job:2")));
        keep(block_on(s().put("job:8", "eight".into())));
        keep(block_on(s().put_until("rline:t:1", "lines".into(), 1_790_000_000_000)));
        for _ in 0..4 { keep(block_on(s().migrate(2)).map(|_| ())) }
        keep(block_on(s().list_last("job:", 2)).map(|_| ()));
        errors
    }

    /// Each of DynamoDB's calls in turn answered with an error it is not asked again for: the store says it, as
    /// DynamoDB said it, and the operations after it go on.
    /// (Also with DynamoDB leaving every batch's keys for later but one. The clean runs alone unless
    /// `SUPERCI_SWEEP_FAULTS=1`, each `SUPERCI_SWEEP_SLICE` its share, as the other sweeps.)
    #[test]
    fn the_store_holds_with_each_call_and_store_operation_failing() {
        let (k0, n) = superci_testkit::sweep::slice();
        let faults = std::env::var("SUPERCI_SWEEP_FAULTS").is_ok_and(|v| v == "1");
        for unprocessed in [false, true] {
            let clean = FakeDynamo { unprocessed_always: RefCell::new(unprocessed), ..Default::default() };
            assert_eq!(every_operation(&clean), Vec::<String>::new());
            let calls = if faults { *clean.calls.borrow() } else { 0 };
            for k in (1..=calls).filter(|k| (*k as usize - 1) % n == k0 - 1) {
                let fake = FakeDynamo { unprocessed_always: RefCell::new(unprocessed), ..Default::default() };
                *fake.fail_at.borrow_mut() = Some(k);
                let errors = every_operation(&fake);
                assert!(errors.iter().all(|e| e.starts_with("ValidationException")), "call {k}: {errors:?}");
            }
        }
    }

    /// What DynamoDB may answer that is not a record (an item without its key or value), an empty page with more
    /// after it, a batch with nothing of what was asked, and throttling that does not end.
    #[test]
    fn the_store_reads_what_dynamodb_answers_amiss_and_stops_asking_again() {
        let fake = FakeDynamo::default();
        let s = store(&fake);
        // (Partition "job", no value; "s", no key: neither is a record.)
        fake.items.borrow_mut().insert(("job".into(), "job:0".into()), json!({ "p": { "S": "job" }, "k": { "S": "job:0" } }));
        fake.items.borrow_mut().insert(("s".into(), "job:x".into()), json!({ "p": { "S": "s" }, "v": { "S": "no key" } }));
        assert_eq!(block_on(s.list("job:")).unwrap(), vec![]);
        assert_eq!(block_on(s.get_many(&["job:0".into()])).unwrap(), [None]);
        assert!(!block_on(s.migrate(5)).unwrap() || fake.items.borrow().contains_key(&("layout".to_string(), "layout".to_string())));
        assert_eq!(block_on(s.list_last("job:", 0)).unwrap(), vec![]);
        // A page that says nothing, more after it.
        let fake = FakeDynamo::default();
        fake.items.borrow_mut().insert(("s".into(), "job:1".into()), json!({ "p": { "S": "s" }, "k": { "S": "job:1" }, "v": { "S": "one" } }));
        *fake.empty_page_once.borrow_mut() = true;
        assert!(!block_on(store(&fake).migrate(5)).unwrap(), "more to come");
        // A batch DynamoDB keeps leaving for later: asked eight times, what it gave kept.
        let fake = FakeDynamo::default();
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "kinds" } }));
        for i in 0..10 { fake.items.borrow_mut().insert(("job".into(), format!("job:{i}")), json!({ "p": { "S": "job" }, "k": { "S": format!("job:{i}") }, "v": { "S": "v" } })); }
        *fake.unprocessed_always.borrow_mut() = true;
        let got = block_on(store(&fake).get_many(&(0..10).map(|i| format!("job:{i}")).collect::<Vec<_>>())).unwrap();
        assert_eq!(got.iter().filter(|g| g.is_some()).count(), 10, "the rest read one by one");
        // Throttled every time: asked again ten times, then what DynamoDB said.
        let fake = FakeDynamo::default();
        *fake.throttled.borrow_mut() = true;
        assert!(block_on(store(&fake).get("job:1")).unwrap_err().starts_with("ThrottlingException"));
        assert_eq!(*fake.calls.borrow(), 10);
    }

    /// An address answering each request as given (its first line and headers handed back): a redirect, a header that
    /// is not text, a body cut short.
    fn answering(answer: &'static [u8]) -> (String, std::sync::mpsc::Receiver<String>) {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let at = format!("http://{}", listener.local_addr().unwrap());
        let (tx, rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || for mut c in listener.incoming().flatten() {
            use std::io::{Read, Write};
            let mut head = [0u8; 4096];
            let n = c.read(&mut head).unwrap_or(0);
            let _ = tx.send(String::from_utf8_lossy(&head[..n]).to_string());
            let _ = c.write_all(answer);
        });
        (at, rx)
    }

    /// The client as the function uses it: its headers sent as given (a host too, when nothing stands in for the
    /// address), a redirect given back when asked not to follow it, a header that is not text left out; what cannot be
    /// asked, or is answered short, said.
    #[test]
    fn the_client_sends_what_it_is_given_and_says_what_went_wrong() {
        let http = Blocking(agent(10), agent(0));
        let (at, asked) = answering(b"HTTP/1.1 302 Found\r\nlocation: https://logs.example/1\r\nx-odd: \xff\xfe\r\ncontent-length: 0\r\nconnection: close\r\n\r\n");
        let r = block_on(http.send(Request::new("GET", &format!("{at}/log")).with_header("host", "example.com").with_header("x-a", "1").without_following())).unwrap();
        assert_eq!((r.status, r.header("location"), r.header("x-odd")), (302, Some("https://logs.example/1"), None));
        let head = asked.recv().unwrap().to_ascii_lowercase();
        assert!(head.contains("host: example.com") && head.contains("x-a: 1"), "{head}");
        let (short, _) = answering(b"HTTP/1.1 200 OK\r\ncontent-length: 1000\r\nconnection: close\r\n\r\nshort");
        assert!(block_on(http.send(Request::new("GET", &short))).is_err());
        assert!(block_on(http.send(Request::new("GET", "http://not a host/"))).is_err());
        let closed = { let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap(); format!("http://{}", l.local_addr().unwrap()) };
        assert!(block_on(http.send(Request::new("POST", &closed))).unwrap_err().starts_with("POST http://127.0.0.1:"));
    }

    /// A call to itself that cannot be signed (a region no address is made of): not sent, its note taken away.
    #[test]
    fn a_call_to_itself_that_cannot_be_signed_is_left_to_the_schedule() {
        let fake = FakeDynamo::default();
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "kinds" } }));
        let s = store(&fake);
        let (lambda, later) = (Lambda::default(), RefCell::new(vec![]));
        let timer = Scheduled { http: &lambda, store: &s, region: "us east-1", function: "superci-plane-t", later: &later };
        block_on(timer.wake_in(1_000)).unwrap();
        assert!(lambda.called.borrow().is_empty());
        assert_eq!(block_on(s.get(WAKING)).unwrap(), None);
    }

    /// A control plane over the store as it runs in AWS, its machines containers of its own (so that DynamoDB is what is
    /// counted), GitHub as the test kit has it.
    #[derive(Default)]
    struct Own(RefCell<Vec<String>>);
    #[async_trait(?Send)]
    impl superci_core::io::Containers for Own {
        async fn start(&self, name: &str, _work: &superci_core::io::Work, _max: u32, _size: superci_core::spec::Size) -> io::Result<String> { self.0.borrow_mut().push(format!("start {name}")); Ok(name.to_string()) }
        async fn stop(&self, id: &str) -> io::Result<()> { self.0.borrow_mut().push(format!("stop {id}")); Ok(()) }
    }
    impl Own { fn stopped(&self, runner: &str) -> bool { self.0.borrow().iter().any(|c| *c == format!("stop {runner}")) } }

    struct At(std::cell::Cell<u64>);
    impl Clock for At { fn now_ms(&self) -> u64 { self.0.get() } }

    struct Later;
    #[async_trait(?Send)]
    impl Timer for Later { async fn wake_in(&self, _ms: u64) -> io::Result<()> { Ok(()) } }

    fn webhook(plane: &ControlPlane<'_>, (body, signature): (Vec<u8>, String)) -> u16 {
        block_on(plane.handle(Request::new("POST", "https://plane.example/webhook").with_header("x-github-event", "workflow_job").with_header("x-hub-signature-256", &signature).with_body(body))).status
    }

    fn github_plane() -> Config {
        let mut config = Config::new(superci_testkit::PLANE.into());
        config.read_settings(&[("GITHUB_APP".to_string(), superci_testkit::GitHub::app().to_string())]);
        config.containers = true;
        // (Room for every job going: a job beside them starts its own machine, not waits.)
        config.routing.order = vec![superci_core::plane::Pool { max_jobs: Some(100_000), ..superci_core::plane::Pool::of("cloudflare") }];
        config
    }

    fn runner_of(id: u64) -> String { format!("superci-{}-{id}", superci_testkit::PLANE) }

    /// The jobs going: one by GitHub's events, and copies of its records (their ids apart) to make `going`, which the
    /// sweep that follows sets right.
    fn going(fake: &FakeDynamo, plane: &ControlPlane<'_>, github: &superci_testkit::GitHub, now: u64, going: u64) {
        let (first, queued) = github.queue("first", "superci", now);
        assert!(webhook(plane, queued) < 300 && webhook(plane, github.start(first)) < 300);
        let one = first.to_string();
        let copies: Vec<((String, String), Value)> = fake.items.borrow().iter()
            .filter(|((_, k), _)| k.contains(&one) && ["job:", "live:", "runner:", "recent:"].iter().any(|p| k.starts_with(p)))
            .flat_map(|((_, k), item)| { let one = &one; (1..going).map(move |n| {
                let id = (80_000_000_000 + n).to_string();
                let k = k.replace(one, &id);
                let mut item: Value = serde_json::from_str(&item.to_string().replace(one, &id)).unwrap();
                item["p"] = json!({ "S": part(&k) });
                ((part(&k), k), item)
            }) }).collect();
        fake.items.borrow_mut().extend(copies);
    }

    /// This month's day totals as a busy month leaves them, late in the day: every record of every day so far with every
    /// hour's waits kept in full (some 12 KB each), and a little spent on each (under the limit on the day's spending).
    fn busy_month(s: &Dynamo<'_>, now: u64) {
        let hour = superci_core::plane::Hour { runs: 120, jobs: 800, ran: 800, not_ran: 3, ms: 96_000_000, billed: 1_700, usd: 31.7, host_usd: 74.4, waits: (0..64u64).map(|i| 4_000 + i * 977 % 90_000).collect() };
        let day = superci_core::plane::Day { spend: [("aws".to_string(), 20.0), ("aws-on-demand".to_string(), 1.0)].into_iter().collect(), hours: vec![hour; 24] };
        let text = serde_json::to_string(&day).unwrap();
        for d in superci_core::plane::month_start_ms(now) / 86_400_000..=now / 86_400_000 {
            for k in superci_core::plane::day_keys(d) { block_on(s.put(&k, text.clone())).unwrap() }
        }
    }

    /// Jobs that ended a while ago that the sweep still cares for, as many as a busy hour leaves: AWS machines whose sent
    /// data CloudWatch counts twelve minutes after they ended, `waiting` of them ended 3 to 12 minutes ago, `due` 12 to 13
    /// minutes ago. Each its record and its entry in `care:`, saying when it is next to be seen to.
    fn care_for(fake: &FakeDynamo, waiting: u64, due: u64, now: u64) {
        let mut items = fake.items.borrow_mut();
        for n in 0..waiting + due {
            let id = 70_000_000_000 + n;
            let ended = if n < waiting { now - 3 * 60_000 - n * 9 * 60_000 / waiting.max(1) } else { now - 12 * 60_000 - 1 - (n - waiting) * 60_000 / due.max(1) };
            let job = json!({ "job_id": id, "run_id": 3, "repo": "acme/app", "state": "done", "at_ms": ended - 600_000, "installation_id": 42, "runner": runner_of(id), "runner_id": 5,
                "cloud": "aws", "machine_id": format!("i-{id:x}"), "machine_type": "m8a.xlarge", "error": null, "seen_in_progress": true, "label": "superci", "usd_per_hour": 0.0712,
                "started_ms": ended - 540_000, "ended_ms": ended, "launched_ms": ended - 600_000, "cpu": 4, "region": "us-east-1", "zone": "us-east-1b", "disk_gb": 60, "began": true,
                "conclusion": "success", "cost_usd": 0.0119, "cost_from": "prices", "counted_usd": 0.0119, "app_id": 99, "rev": 6, "name": "unit (3)", "workflow": "Lint & Test",
                "queued_ms": ended - 610_000, "branch": "main", "default_branch": "main", "repo_id": 77 });
            for (k, v) in [(format!("job:{id}"), job.to_string()), (format!("care:{id}"), (ended + 12 * 60_000).to_string())] {
                items.insert((part(&k), k.clone()), json!({ "p": { "S": part(&k) }, "k": { "S": k }, "v": { "S": v } }));
            }
        }
    }

    /// The busiest partition of what `units` took, by reads and by writes, as a share of what DynamoDB lets one take
    /// a second (3,000 read units, 1,000 write units) at `per_second` of what `units` is one of.
    fn busiest(units: &BTreeMap<String, (f64, f64)>, per_second: f64) -> String {
        let most = |w: bool| units.iter().map(|(p, (r, wr))| (p.clone(), if w { *wr } else { *r })).fold((String::new(), 0.0), |a, b| if b.1 > a.1 { b } else { a });
        let ((rp, r), (wp, w)) = (most(false), most(true));
        format!("reads {rp} {:.0} RCU/s ({:.1}% of 3,000), writes {wp} {:.0} WCU/s ({:.1}% of 1,000)", r * per_second, r * per_second / 30.0, w * per_second, w * per_second / 10.0)
    }

    /// What GitHub's events cost DynamoDB, as DynamoDB counts it (read units of 4 KB, half for an eventually consistent
    /// read; write units of 1 KB), with 50 jobs going and with 2,000: about the same, as an event reads and writes its own
    /// job's records and none that grow with the jobs going. As production runs: a start weighs two places (as AWS's
    /// spot and on-demand machines), a limit on the day's spending is set, this month's day totals are as a busy month
    /// leaves them, and the sweep cares for as many ended jobs as a busy hour leaves. And no partition takes a large part
    /// of a busy minute's: at 50 events a second (a job's three), each partition's share stays under a quarter of what
    /// DynamoDB lets one take (3,000 read units, 1,000 write units a second).
    #[test]
    fn an_event_costs_dynamodb_the_same_with_fifty_jobs_going_or_two_thousand() {
        let mut costs: Vec<Vec<(f64, f64)>> = vec![];
        let mut said = vec![];
        for going_now in [50u64, 2_000] {
            let fake = FakeDynamo { calm: RefCell::new(true), ..Default::default() };
            fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "spread" } }));
            let (s, github, own, cache) = (store(&fake), superci_testkit::GitHub::default(), Own::default(), Cache::default());
            let mut config = github_plane();
            // (AWS's place is not connected here: it is passed over, and what runs on it read all the same.)
            config.routing.order.insert(0, superci_core::plane::Pool::of("aws"));
            config.routing.daily_usd = Some(1_500.0);
            let clock = At(std::cell::Cell::new(1_790_000_000_000));
            let plane = ControlPlane { store: &s, http: &github, clock: &clock, timer: &Later, config: &config, cache: &cache, containers: Some(&own) };
            block_on(plane.alarm()).unwrap();
            going(&fake, &plane, &github, clock.now_ms(), going_now);
            busy_month(&s, clock.now_ms());
            care_for(&fake, 9_000, 1_000, clock.now_ms());
            block_on(plane.alarm()).unwrap();
            assert_eq!(block_on(s.list("live:")).unwrap().len() as u64, going_now);
            let take = || std::mem::take(&mut *fake.units.borrow_mut());
            let total = |u: &BTreeMap<String, (f64, f64)>| (u.values().map(|(r, _)| r).sum::<f64>(), u.values().map(|(_, w)| w).sum::<f64>());
            // Jobs beside them, each queued (and given its machine), begun and ended: what each event cost, and over them all,
            // what each partition took a job.
            let (mut each, mut by_part, jobs) = (vec![(0.0, 0.0); 3], BTreeMap::<String, (f64, f64)>::new(), 32.0);
            take();
            for _ in 0..jobs as u64 {
                let (id, queued) = github.queue("beside", "superci", clock.now_ms());
                let events = [queued, github.start(id), github.finish(id, "success", "ok", clock.now_ms())];
                for (i, event) in events.into_iter().enumerate() {
                    assert!(webhook(&plane, event) < 300);
                    let u = take();
                    let (r, w) = total(&u);
                    (each[i].0, each[i].1) = (each[i].0 + r / jobs, each[i].1 + w / jobs);
                    for (p, (r, w)) in u { let t = by_part.entry(p).or_default(); (t.0, t.1) = (t.0 + r / jobs, t.1 + w / jobs) }
                }
            }
            let per_second = 50.0 / 3.0;
            let (r, w) = (each.iter().map(|e| e.0).sum::<f64>() * per_second, each.iter().map(|e| e.1).sum::<f64>() * per_second);
            said.push(format!("capacity: {going_now} going: queued {:.1} R / {:.1} W, in_progress {:.1} R / {:.1} W, completed {:.1} R / {:.1} W a job; at 50 events/s the table {r:.0} RCU/s and {w:.0} WCU/s, busiest {}",
                each[0].0, each[0].1, each[1].0, each[1].1, each[2].0, each[2].1, busiest(&by_part, per_second)));
            eprintln!("{}", said.last().unwrap());
            for (p, (r, w)) in &by_part { assert!(r * per_second < 750.0 && w * per_second < 250.0, "{going_now} going: partition {p} takes {r:.1} read and {w:.1} write units a job: {by_part:?}") }
            costs.push(each);
        }
        for (i, event) in ["queued and started", "in progress", "completed"].iter().enumerate() {
            let ((r50, w50), (r2k, w2k)) = (costs[0][i], costs[1][i]);
            assert!(r2k <= r50 * 1.25 + 2.0 && w2k <= w50 * 1.25 + 2.0, "{event}: {r50:.1} read and {w50:.1} write units with 50 jobs going, {r2k:.1} and {w2k:.1} with 2,000: {costs:?}");
        }
    }

    /// DynamoDB throttling or leaving reads for later never ends a machine. Job 1's machine runs job 2 (GitHub gave it the
    /// job; its event comes last) when job 1 ends on another runner. While DynamoDB throttles every list and batch of
    /// reads, then every read of one record, then leaves batches for later, each request either does nothing or what it
    /// would have done: the machine stays until job 2's end says its runner is done.
    #[test]
    fn throttled_reads_or_reads_left_for_later_end_no_machine() {
        let fake = FakeDynamo { calm: RefCell::new(true), ..Default::default() };
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "spread" } }));
        let (s, github, own, cache, config) = (store(&fake), superci_testkit::GitHub::default(), Own::default(), Cache::default(), github_plane());
        let clock = At(std::cell::Cell::new(1_790_000_000_000));
        let plane = ControlPlane { store: &s, http: &github, clock: &clock, timer: &Later, config: &config, cache: &cache, containers: Some(&own) };
        block_on(plane.alarm()).unwrap();
        let (one, queued) = github.queue("one", "superci", clock.now_ms());
        assert!(webhook(&plane, queued) < 300);
        let (two, queued) = github.queue("two", "superci", clock.now_ms());
        assert!(webhook(&plane, queued) < 300);
        let rid = block_on(s.get(&format!("job:{one}"))).unwrap().and_then(|j| serde_json::from_str::<Value>(&j).ok()).and_then(|j| j["runner_id"].as_u64()).unwrap();
        github.fail("DELETE", &format!("/runners/{rid}"), 422, "Runner is still running a job");
        let began = github.start_on(two, &runner_of(one));
        let _ = github.start_elsewhere(one, "someone-elses-runner");
        let ended = github.finish(one, "success", "ok", clock.now_ms());
        let sweep = || { clock.0.set(clock.0.get() + 4 * 60_000); let _ = block_on(plane.alarm()); };
        *fake.throttle_ops.borrow_mut() = vec!["Query", "BatchGetItem"];
        let _ = webhook(&plane, ended.clone());
        sweep();
        assert!(!own.stopped(&runner_of(one)), "lists throttled: {:?}", own.0.borrow());
        *fake.throttle_ops.borrow_mut() = vec!["GetItem"];
        let _ = webhook(&plane, ended.clone());
        sweep();
        assert!(!own.stopped(&runner_of(one)), "reads of a record throttled: {:?}", own.0.borrow());
        fake.throttle_ops.borrow_mut().clear();
        *fake.unprocessed_always.borrow_mut() = true;
        assert!(webhook(&plane, ended) < 300);
        sweep();
        assert!(!own.stopped(&runner_of(one)), "batches left for later: {:?}", own.0.borrow());
        assert!(webhook(&plane, began) < 300);
        sweep();
        assert!(!own.stopped(&runner_of(one)));
        github.fail_none();
        assert!(webhook(&plane, github.finish(two, "success", "ok", clock.now_ms())) < 300);
        assert!(own.stopped(&runner_of(one)), "{:?}", own.0.borrow());
    }

    /// GitHub as the test kit has it, and CloudWatch saying what each machine sent; each call the time it takes (see
    /// `HTTP_US`), and the calls to CloudWatch counted.
    struct Modeled<'a> { github: &'a superci_testkit::GitHub, took_us: RefCell<u64>, sent: RefCell<u64> }
    #[async_trait(?Send)]
    impl Http for Modeled<'_> {
        async fn send(&self, r: Request) -> io::Result<Response> {
            *self.took_us.borrow_mut() += HTTP_US;
            if r.url.starts_with("https://monitoring.") {
                *self.sent.borrow_mut() += 1;
                return Ok(Response::new(200, "text/xml", "<GetMetricStatisticsResponse><GetMetricStatisticsResult><Datapoints><member><Sum>52000000</Sum></member></Datapoints></GetMetricStatisticsResult></GetMetricStatisticsResponse>"));
            }
            self.github.send(r).await
        }
    }

    /// The time as the calls made so far took it, one after another as a request makes them: DynamoDB's and the others'.
    struct Lapse<'a> { at: u64, fake: &'a FakeDynamo, http: &'a Modeled<'a> }
    impl Clock for Lapse<'_> { fn now_ms(&self) -> u64 { self.at + (*self.fake.took_us.borrow() + *self.http.took_us.borrow()) / 1_000 } }

    /// The sweep with 2,000 jobs going and as many ended a while ago as a busy hour leaves it to care for (9,000 AWS
    /// machines whose sent data CloudWatch has not counted yet, 1,000 whose it has): what one sweep reads and writes,
    /// and how long it takes at the time each call takes (DynamoDB's and CloudWatch's, see `LATENCY_US`, `HTTP_US`). It
    /// reads what is due and no more, and ends well within the minute and a half its claim holds (and the minute the
    /// function may run): what is left of the sent data to add is added at the sweeps after.
    #[test]
    fn a_sweep_with_two_thousand_jobs_going_and_a_full_care_set_reads_what_is_due_and_ends_in_time() {
        let fake = FakeDynamo { calm: RefCell::new(true), ..Default::default() };
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "spread" } }));
        let (s, github, own, cache) = (store(&fake), superci_testkit::GitHub::default(), Own::default(), Cache::default());
        let clouds = Modeled { github: &github, took_us: RefCell::new(0), sent: RefCell::new(0) };
        let mut config = github_plane();
        (config.aws_own, config.aws_own_creds) = (Some(("111111111111".into(), "us-east-1".into())), Some(Credentials { access_key_id: "ASIAX".into(), secret_access_key: "secret".into(), session_token: Some("tok".into()), expires_at_ms: u64::MAX }));
        // (AWS's places off: its jobs here are those that ended; the jobs going run in containers of the plane's own.)
        config.routing.order = vec![superci_core::plane::Pool { off: true, ..superci_core::plane::Pool::of("aws") }, superci_core::plane::Pool { off: true, ..superci_core::plane::Pool::of(superci_core::plane::AWS_ON_DEMAND) },
            superci_core::plane::Pool { max_jobs: Some(100_000), ..superci_core::plane::Pool::of("cloudflare") }];
        let clock = Lapse { at: 1_790_000_000_000, fake: &fake, http: &clouds };
        let plane = ControlPlane { store: &s, http: &clouds, clock: &clock, timer: &Later, config: &config, cache: &cache, containers: Some(&own) };
        block_on(plane.alarm()).unwrap();
        going(&fake, &plane, &github, clock.now_ms(), 2_000);
        block_on(plane.alarm()).unwrap();
        care_for(&fake, 9_000, 1_000, clock.now_ms());
        fake.units.borrow_mut().clear();
        fake.ops.borrow_mut().clear();
        let (began, real, sent) = (clock.now_ms(), Instant::now(), *clouds.sent.borrow());
        block_on(plane.alarm()).unwrap();
        let (took, cpu, sent) = (clock.now_ms() - began, real.elapsed(), *clouds.sent.borrow() - sent);
        let units = std::mem::take(&mut *fake.units.borrow_mut());
        let (r, w) = (units.values().map(|(r, _)| r).sum::<f64>(), units.values().map(|(_, w)| w).sum::<f64>());
        let jobs_read = units.iter().filter(|(p, _)| p.starts_with("job")).map(|(_, (r, _))| r).sum::<f64>();
        let mut by_kind = BTreeMap::<String, (f64, f64)>::new();
        for (p, (r, w)) in &units { let t = by_kind.entry(p.split('#').next().unwrap_or(p).to_string()).or_default(); (t.0, t.1) = (t.0 + r, t.1 + w) }
        let by_kind: Vec<String> = by_kind.iter().map(|(k, (r, w))| format!("{k} {r:.0} R / {w:.0} W")).collect();
        eprintln!("capacity: the sweep, 2,000 going and 10,000 cared for (1,000 due): {r:.0} RCU ({jobs_read:.0} of them job records) and {w:.0} WCU, {sent} sent data added, {:.1} s as the calls take ({:?} here), calls {:?}; by kind {}; over the sweep {}",
            took as f64 / 1_000.0, cpu, fake.ops.borrow(), by_kind.join(", "), busiest(&units, 1_000.0 / took.max(1) as f64));
        assert!(took < 45_000, "the sweep takes {:.1} s at the time its calls take, past the minute the function may run", took as f64 / 1_000.0);
        assert!(jobs_read < 4_000.0, "{jobs_read:.0} read units of job records: more than the jobs going and those due");
        assert!(sent > 0, "some of the sent data due added");
    }

    /// Each request's own wake-ups: none (the test sweeps itself). A pause: as many turns of the requests going at once
    /// as it is long, one for each ten milliseconds.
    struct Turns;
    #[async_trait(?Send)]
    impl Timer for Turns {
        async fn wake_in(&self, _ms: u64) -> io::Result<()> { Ok(()) }
        async fn pause(&self, ms: u64) { for _ in 0..ms / 10 + 1 { Turn(false).await } }
    }

    /// A matrix's hundreds of jobs queued in one burst, each event in a request of its own and the requests' calls
    /// interleaved, as requests running at once interleave theirs: hundreds of writers on each place's records of what runs.
    /// Every job gets its machine and each machine is in its job's record; once a sweep has been, the records of what
    /// runs hold exactly the jobs going. What those records' writes lost to one another is said.
    #[test]
    fn a_burst_of_jobs_queued_at_once_gets_each_its_machine_in_its_record() {
        let fake = FakeDynamo { calm: RefCell::new(true), ..Default::default() };
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "spread" } }));
        let (s, github, own, cache, config) = (store(&fake), superci_testkit::GitHub::default(), Own::default(), Cache::default(), github_plane());
        let clock = At(std::cell::Cell::new(1_790_000_000_000));
        let plane = ControlPlane { store: &s, http: &github, clock: &clock, timer: &Later, config: &config, cache: &cache, containers: Some(&own) };
        block_on(plane.alarm()).unwrap();
        let n = 300;
        let events: Vec<(u64, (Vec<u8>, String))> = (0..n).map(|i| github.queue(&format!("matrix ({i})"), "superci", clock.now_ms())).collect();
        let ids: Vec<u64> = events.iter().map(|(id, _)| *id).collect();
        let (stores, caches): (Vec<Dynamo<'_>>, Vec<Cache>) = ((0..n).map(|_| store(&fake)).collect(), (0..n).map(|_| Cache::default()).collect());
        *fake.interleaved.borrow_mut() = true;
        fake.lost.borrow_mut().clear();
        let answers = block_on(futures::future::join_all(events.into_iter().zip(&stores).zip(&caches).map(|(((_, (body, signature)), s), cache)| {
            let plane = ControlPlane { store: s, http: &github, clock: &clock, timer: &Turns, config: &config, cache, containers: Some(&own) };
            async move { plane.handle(Request::new("POST", "https://plane.example/webhook").with_header("x-github-event", "workflow_job").with_header("x-hub-signature-256", &signature).with_body(body)).await.status }
        })));
        *fake.interleaved.borrow_mut() = false;
        let lost = std::mem::take(&mut *fake.lost.borrow_mut());
        eprintln!("capacity: a burst of {n} queued at once: conditional writes lost to another, by partition: {lost:?}");
        assert!(answers.iter().all(|a| *a < 300), "{answers:?}");
        let jobs: Vec<Value> = ids.iter().map(|id| serde_json::from_str(&block_on(s.get(&format!("job:{id}"))).unwrap().unwrap()).unwrap()).collect();
        let unplaced: Vec<&Value> = jobs.iter().filter(|j| j["state"] != "launched" || j["machine_id"].as_str() != Some(runner_of(j["job_id"].as_u64().unwrap()).as_str())).collect();
        assert!(unplaced.is_empty(), "{} of {n} jobs have not their machine in their record, the first: {}", unplaced.len(), unplaced[0]);
        let held: std::collections::BTreeSet<String> = block_on(s.list("job:")).unwrap().into_iter().filter_map(|(_, v)| serde_json::from_str::<Value>(&v).ok()?["machine_id"].as_str().map(str::to_string)).collect();
        let started: Vec<String> = own.0.borrow().iter().filter_map(|c| c.strip_prefix("start ").map(str::to_string)).collect();
        assert!(started.iter().all(|m| held.contains(m)), "machines no record holds: {:?}", started.iter().filter(|m| !held.contains(*m)).collect::<Vec<_>>());
        block_on(plane.alarm()).unwrap();
        let running: std::collections::BTreeSet<u64> = block_on(s.list("active:")).unwrap().into_iter()
            .flat_map(|(_, v)| serde_json::from_str::<BTreeMap<String, u64>>(&v).unwrap().into_keys().filter_map(|id| id.parse().ok()).collect::<Vec<u64>>()).collect();
        assert_eq!(running, ids.iter().copied().collect::<std::collections::BTreeSet<u64>>(), "what runs, once the sweep has been");
    }

    /// Moving a record from where a layout before kept it races what requests do meanwhile. One deleted between the
    /// move's read of it and its write (by a request of this version, which deletes it wherever it is) is not brought
    /// back; one written meanwhile by a request of the version before (which writes where that one kept it) keeps what
    /// that request wrote.
    #[test]
    fn a_record_moved_while_it_is_deleted_or_written_stays_deleted_or_as_written() {
        let fake = FakeDynamo::default();
        let at = |k: &str| (("job".to_string(), k.to_string()), json!({ "p": { "S": "job" }, "k": { "S": k }, "v": { "S": "kept" }, "x": { "N": "1790000000" } }));
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "kinds" } }));
        fake.items.borrow_mut().extend([at("job:7"), at("job:8"), at("job:9")]);
        let (mut deleted, mut written) = (false, false);
        *fake.meddle.borrow_mut() = Some(Box::new(move |op: &str, b: &Value, items: &mut BTreeMap<(String, String), Value>| {
            if op == "PutItem" && b["Item"]["k"]["S"] == "job:7" && b["ConditionExpression"] == "attribute_not_exists(k)" && !deleted {
                deleted = true;
                items.remove(&("job".to_string(), "job:7".to_string()));
            }
            if op == "DeleteItem" && b["Key"]["k"]["S"] == "job:8" && b["Key"]["p"]["S"] == "job" && !written {
                written = true;
                if let Some(old) = items.get_mut(&("job".to_string(), "job:8".to_string())) { old["v"] = json!({ "S": "newer" }) }
            }
        }));
        let s = store(&fake);
        while !block_on(s.migrate(10)).unwrap() {}
        assert_eq!(block_on(s.get("job:7")).unwrap(), None, "deleted, and not brought back by the move");
        assert_eq!(block_on(s.get("job:8")).unwrap().as_deref(), Some("newer"), "as the request of the version before wrote it");
        assert_eq!(block_on(s.get("job:9")).unwrap().as_deref(), Some("kept"));
        assert!(fake.items.borrow().keys().all(|(p, _)| p != "job"), "nothing left where a kind was kept whole");
    }

    /// The sweep's claim: one sweep at a time, a claim of one cut off taken over after a minute and a half. A sweep that
    /// ends after another took its claim over gives back only its own: the other's stays, and no third starts beside it.
    #[test]
    fn a_sweep_that_ends_after_another_took_its_claim_over_leaves_that_ones_claim() {
        let fake = FakeDynamo::default();
        fake.items.borrow_mut().insert(("layout".into(), "layout".into()), json!({ "p": { "S": "layout" }, "k": { "S": "layout" }, "v": { "S": "spread" } }));
        let s = store(&fake);
        let t = 1_790_000_000_000;
        let first = sweep_claim(&s, t).unwrap().expect("none going");
        assert!(sweep_claim(&s, t + 30_000).unwrap().is_none(), "one going");
        let second = sweep_claim(&s, t + 91_000).unwrap().expect("the first cut off, as far as can be told");
        sweep_done(&s, &first);
        assert_eq!(block_on(s.get(SWEEPING)).unwrap(), Some(second.clone()), "the second's claim kept");
        assert!(sweep_claim(&s, t + 92_000).unwrap().is_none(), "no third beside the second");
        sweep_done(&s, &second);
        assert!(sweep_claim(&s, t + 93_000).unwrap().is_some(), "free once the second is done");
    }
}
