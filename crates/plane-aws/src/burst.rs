//! A matrix's jobs queued at once, run as AWS runs the control plane, in the harness's own time. Each event is a Lambda
//! invocation (see `invoke`) in a container of its own while others are busy: a new container reads its secrets from
//! SSM first, which takes 40 reads a second. DynamoDB takes 1,000 write units and 3,000 read units a second in each of
//! the table's partitions (four, as the table's warm throughput says) and in any one item, and throttles what is over.
//! Lambda ends an invocation at 60 seconds and calls an asynchronous one again twice, a minute apart. GitHub counts an
//! answer later than ten seconds, or of 5xx, as a failed delivery, and lists the newest hundred. A machine takes 1.5 s
//! to be asked for and 15 s to boot; its runner then takes the oldest job queued, which runs 60 to 120 seconds.
use super::*;
use std::cell::{Cell, RefCell};
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use superci_core::plane::Pool;

const PLANE: &str = superci_testkit::PLANE;
const TABLE: &str = "superci-plane-t";
const URL: &str = "https://plane.example/webhook";
const T0: u64 = 1_790_000_000_000;

const PARTITIONS: u64 = 4;
const READ_UNITS: f64 = 3_000.0;
const WRITE_UNITS: f64 = 1_000.0;
const SSM_PER_SECOND: u32 = 40;
const LAMBDA_MS: u64 = 60_000;
const GITHUB_WAITS_MS: u64 = 10_000;
const LAUNCH_MS: u64 = 1_500;
const BOOT_MS: u64 = 15_000;

/// The harness's clock (ms). A wait is a `Nap`; when every invocation waits, the run moves the clock on to the soonest.
struct Sim { now: Cell<u64>, due: Cell<u64> }

impl Sim {
    fn after(&self, ms: u64) -> Nap<'_> { Nap { sim: self, until: self.now.get() + ms } }
}

struct Nap<'a> { sim: &'a Sim, until: u64 }

impl Future for Nap<'_> {
    type Output = ();
    fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<()> {
        if self.sim.now.get() >= self.until { return Poll::Ready(()) }
        self.sim.due.set(self.sim.due.get().min(self.until));
        Poll::Pending
    }
}

impl Clock for Sim { fn now_ms(&self) -> u64 { self.now.get() } }

#[async_trait(?Send)]
impl Naps for Sim { async fn nap(&self, ms: u64) { self.after(ms).await } }

fn fnv(s: &str) -> u64 { s.bytes().fold(0xcbf2_9ce4_8422_2325u64, |h, b| (h ^ b as u64).wrapping_mul(0x100_0000_01b3)) }

/// An item's size as DynamoDB counts it: its attributes' names and values.
fn size(item: &Value) -> usize { item.as_object().map_or(0, |o| o.iter().map(|(k, v)| k.len() + v.as_object().and_then(|t| t.values().next()).and_then(Value::as_str).map_or(0, str::len)).sum()) }

/// DynamoDB, with what each partition and each item takes in a second.
#[derive(Default)]
struct Table {
    items: RefCell<BTreeMap<(String, String), Value>>,
    used: RefCell<HashMap<(String, u64), (f64, f64)>>,
    /// Throttled calls, by item (`partition key | sort key`, a query's by its prefix) and by the partition they were in.
    throttled: RefCell<BTreeMap<String, u64>>,
    throttled_in: RefCell<BTreeMap<String, u64>>,
    /// Read and write units each item took, all told.
    read: RefCell<BTreeMap<String, f64>>,
    wrote: RefCell<BTreeMap<String, f64>>,
}

impl Table {
    fn partition(p: &str) -> String { format!("partition {}", fnv(p) % PARTITIONS) }

    /// Takes `read` and `write` units from the partition `p` is in and from the item, this second; or throttles.
    fn take(&self, at: u64, p: &str, k: &str, read: f64, write: f64) -> bool {
        let second = at / 1000;
        let (part, item) = (Self::partition(p), format!("{p}|{k}"));
        let mut used = self.used.borrow_mut();
        let in_part = used.get(&(part.clone(), second)).copied().unwrap_or_default();
        let in_item = used.get(&(item.clone(), second)).copied().unwrap_or_default();
        if in_part.0 + read > READ_UNITS || in_part.1 + write > WRITE_UNITS || in_item.0 + read > READ_UNITS || in_item.1 + write > WRITE_UNITS {
            *self.throttled.borrow_mut().entry(item).or_default() += 1;
            *self.throttled_in.borrow_mut().entry(part).or_default() += 1;
            return false
        }
        for b in [part, item.clone()] { let u = used.entry((b, second)).or_default(); u.0 += read; u.1 += write }
        if read > 0.0 { *self.read.borrow_mut().entry(item.clone()).or_default() += read }
        if write > 0.0 { *self.wrote.borrow_mut().entry(item).or_default() += write }
        true
    }

    async fn call(&self, sim: &Sim, r: Request) -> io::Result<Response> {
        let op = r.header("x-amz-target").unwrap_or_default().trim_start_matches("DynamoDB_20120810.").to_string();
        sim.after(match op.as_str() { "GetItem" => 4, "Query" => 8, "BatchGetItem" => 12, _ => 6 }).await;
        let now = sim.now.get();
        let b: Value = serde_json::from_slice(&r.body).unwrap_or_default();
        let fail = |kind: &str| Ok(Response::new(400, "application/x-amz-json-1.0", json!({ "__type": format!("com.amazonaws.dynamodb.v20120810#{kind}"), "message": kind }).to_string()));
        let ok = |v: Value| Ok(Response::new(200, "application/x-amz-json-1.0", v.to_string()));
        let key = |k: &Value| (k["p"]["S"].as_str().unwrap_or_default().to_string(), k["k"]["S"].as_str().unwrap_or_default().to_string());
        let reads = |bytes: usize, consistent: bool| bytes.div_ceil(4096).max(1) as f64 * if consistent { 1.0 } else { 0.5 };
        let writes = |bytes: usize| bytes.div_ceil(1024).max(1) as f64;
        let mut items = self.items.borrow_mut();
        match op.as_str() {
            "GetItem" => {
                let at = key(&b["Key"]);
                if !self.take(now, &at.0, &at.1, reads(items.get(&at).map_or(0, size), b["ConsistentRead"] == true), 0.0) { return fail("ProvisionedThroughputExceededException") }
                ok(match items.get(&at) { Some(i) => json!({ "Item": i }), None => json!({}) })
            }
            "PutItem" => {
                let at = key(&b["Item"]);
                if !self.take(now, &at.0, &at.1, 0.0, writes(size(&b["Item"]).max(items.get(&at).map_or(0, size)))) { return fail("ProvisionedThroughputExceededException") }
                let held = items.get(&at);
                let allowed = match b["ConditionExpression"].as_str() {
                    Some("attribute_not_exists(k)") => held.is_none(),
                    Some("v = :was") => held.is_some_and(|h| h["v"] == b["ExpressionAttributeValues"][":was"]),
                    Some(other) => panic!("a condition the harness does not know: {other}"),
                    None => true,
                };
                if !allowed { return fail("ConditionalCheckFailedException") }
                items.insert(at, b["Item"].clone());
                ok(json!({}))
            }
            "DeleteItem" => {
                let at = key(&b["Key"]);
                if !self.take(now, &at.0, &at.1, 0.0, writes(items.get(&at).map_or(0, size))) { return fail("ProvisionedThroughputExceededException") }
                if b["ConditionExpression"] == "v = :was" && items.get(&at).is_none_or(|h| h["v"] != b["ExpressionAttributeValues"][":was"]) { return fail("ConditionalCheckFailedException") }
                items.remove(&at);
                ok(json!({}))
            }
            "Query" => {
                let vals = &b["ExpressionAttributeValues"];
                let (p, prefix) = (vals[":p"]["S"].as_str().unwrap_or_default(), vals[":k"]["S"].as_str().unwrap_or(""));
                let mut hits: Vec<&Value> = items.range((p.to_string(), prefix.to_string())..).take_while(|((ip, ik), _)| ip == p && ik.starts_with(prefix)).map(|(_, i)| i).collect();
                if b["ScanIndexForward"] == false { hits.reverse() }
                let start = b["ExclusiveStartKey"]["k"]["S"].as_str().map_or(0, |a| hits.iter().position(|i| i["k"]["S"].as_str() == Some(a)).map_or(0, |x| x + 1));
                let page = b["Limit"].as_u64().map_or(1_000, |l| (l as usize).min(1_000));
                let shown: Vec<Value> = hits.iter().skip(start).take(page).map(|i| (*i).clone()).collect();
                if !self.take(now, p, prefix, reads(shown.iter().map(size).sum(), b["ConsistentRead"] == true), 0.0) { return fail("ProvisionedThroughputExceededException") }
                let more = start + page < hits.len();
                ok(if more { json!({ "Items": shown, "LastEvaluatedKey": { "p": { "S": p }, "k": shown.last().unwrap()["k"] } }) } else { json!({ "Items": shown }) })
            }
            "BatchGetItem" => {
                let table = b["RequestItems"].as_object().unwrap().keys().next().unwrap().clone();
                let consistent = b["RequestItems"][&table]["ConsistentRead"] == true;
                let (mut found, mut left) = (vec![], vec![]);
                for k in b["RequestItems"][&table]["Keys"].as_array().cloned().unwrap_or_default() {
                    let at = key(&k);
                    if !self.take(now, &at.0, &at.1, reads(items.get(&at).map_or(0, size), consistent), 0.0) { left.push(k); continue }
                    if let Some(i) = items.get(&at) { found.push(i.clone()) }
                }
                if found.is_empty() && !left.is_empty() && left.len() == b["RequestItems"][&table]["Keys"].as_array().map_or(0, Vec::len) { return fail("ProvisionedThroughputExceededException") }
                ok(json!({ "Responses": { &table: found }, "UnprocessedKeys": if left.is_empty() { json!({}) } else { json!({ &table: { "Keys": left, "ConsistentRead": consistent } }) } }))
            }
            other => panic!("a call the harness does not take: {other}"),
        }
    }
}

/// What the plane reaches over HTTP: DynamoDB, SSM, Lambda (its calls to itself, kept for the run to start) and GitHub.
struct Router<'w> { sim: &'w Sim, table: &'w Table, github: &'w superci_testkit::GitHub, ssm: Cell<(u64, u32)>, invoked: RefCell<Vec<Value>> }

#[async_trait(?Send)]
impl Http for Router<'_> {
    async fn send(&self, r: Request) -> io::Result<Response> {
        let host = r.url.split("//").nth(1).unwrap_or_default().split('/').next().unwrap_or_default().to_string();
        if host.starts_with("dynamodb.") { return self.table.call(self.sim, r).await }
        if host.starts_with("ssm.") {
            self.sim.after(15).await;
            let second = self.sim.now.get() / 1000;
            let (at, n) = self.ssm.get();
            let n = if at == second { n } else { 0 };
            if n >= SSM_PER_SECOND { return Ok(Response::new(400, "application/x-amz-json-1.1", json!({ "__type": "ThrottlingException", "message": "Rate exceeded" }).to_string())) }
            self.ssm.set((second, n + 1));
            let app = superci_testkit::GitHub::app().to_string();
            return Ok(Response::new(200, "application/x-amz-json-1.1", json!({ "Parameters": [{ "Name": format!("/superci/{PLANE}/GITHUB_APP"), "Value": app }] }).to_string()))
        }
        if host.starts_with("lambda.") {
            self.sim.after(15).await;
            self.invoked.borrow_mut().push(serde_json::from_slice(&r.body).unwrap_or_default());
            return Ok(Response::new(202, "application/json", "{}"))
        }
        self.sim.after(60).await;
        self.github.send(r).await
    }
}

/// The machines: each asked for, booted, and stopped at the harness's times.
struct Machines<'w> { sim: &'w Sim, booting: RefCell<Vec<(u64, String)>>, stopped: RefCell<BTreeMap<String, u64>>, started: RefCell<BTreeSet<String>> }

#[async_trait(?Send)]
impl superci_core::io::Containers for Machines<'_> {
    async fn start(&self, name: &str, _work: &superci_core::io::Work, _max: u32, _size: superci_core::spec::Size) -> io::Result<String> {
        self.sim.after(LAUNCH_MS).await;
        self.started.borrow_mut().insert(name.to_string());
        self.booting.borrow_mut().push((self.sim.now.get() + BOOT_MS, name.to_string()));
        Ok(name.to_string())
    }
    async fn stop(&self, id: &str) -> io::Result<()> {
        self.sim.after(300).await;
        self.stopped.borrow_mut().entry(id.to_string()).or_insert(self.sim.now.get());
        Ok(())
    }
}

/// A Lambda container: its secrets as last read, and its cache.
struct Warm { secrets: RefCell<Secrets>, cache: Cache }

enum Call { Hook(Vec<u8>, String), Async(Value, u32) }

enum Event { Call(Call), Boot(String), Finish(u64, String) }

struct World<'w> {
    sim: &'w Sim, router: &'w Router<'w>, machines: &'w Machines<'w>, github: &'w superci_testkit::GitHub, configure: &'w dyn Fn(&Secrets, &str) -> Config,
    idle: RefCell<Vec<Rc<Warm>>>, busy: Cell<usize>, most: Cell<usize>, cold: Cell<u64>,
    events: RefCell<BTreeMap<u64, Vec<Event>>>,
    errors: RefCell<BTreeMap<String, u64>>, timeouts: RefCell<BTreeMap<String, u64>>, failed_deliveries: Cell<u64>,
}

struct Task<'w> { run: Pin<Box<dyn Future<Output = ()> + 'w>>, wake_at: u64, deadline: u64, done: bool, kind: String, retry: Option<(Value, u32)>, hook: Option<(Vec<u8>, Rc<Cell<bool>>)> }

/// A container in use: given back when its invocation ends, dropped with one Lambda ended (it starts cold again).
struct Held<'w> { world: &'w World<'w>, warm: Rc<Warm>, done: bool }
impl Drop for Held<'_> {
    fn drop(&mut self) {
        self.world.busy.set(self.world.busy.get() - 1);
        if self.done { self.world.idle.borrow_mut().push(self.warm.clone()) }
    }
}

impl<'w> World<'w> {
    fn at(&self, t: u64, e: Event) { self.events.borrow_mut().entry(t).or_default().push(e) }

    fn invocation(&'w self, call: Call) -> Task<'w> {
        let started = self.sim.now.get();
        let warm = self.idle.borrow_mut().pop().unwrap_or_else(|| { self.cold.set(self.cold.get() + 1); Rc::new(Warm { secrets: RefCell::new(Secrets { at: None, values: vec![] }), cache: Cache::default() }) });
        self.busy.set(self.busy.get() + 1);
        self.most.set(self.most.get().max(self.busy.get()));
        let (event, kind, retry, hook) = match call {
            Call::Hook(body, signature) => {
                let action = serde_json::from_slice::<Value>(&body).ok().and_then(|v| v["action"].as_str().map(str::to_string)).unwrap_or_default();
                let event = json!({ "requestContext": { "domainName": "plane.example", "http": { "method": "POST" } }, "rawPath": "/webhook", "rawQueryString": "",
                    "headers": { "x-github-event": "workflow_job", "x-hub-signature-256": signature, "content-type": "application/json" },
                    "body": String::from_utf8(body.clone()).unwrap(), "isBase64Encoded": false });
                (event, action, None, Some((body, Rc::new(Cell::new(false)))))
            }
            Call::Async(payload, attempt) => { let kind = payload["superci"].as_str().unwrap_or("async").to_string(); (payload.clone(), kind, Some((payload, attempt)), None) }
        };
        let (retry_in, hook_in) = (retry.clone(), hook.clone());
        let run = Box::pin(async move {
            let mut held = Held { world: self, warm, done: false };
            let warm = held.warm.clone();
            let env = Env { http: self.router, clock: self.sim, naps: self.sim, region: "us-east-1", table: TABLE, plane_id: PLANE, function: TABLE, account_id: "123456789012",
                cache: &warm.cache, containers: Some(self.machines), configure: self.configure };
            let mut secrets = warm.secrets.borrow_mut();
            invoke(&env, &mut *secrets, &event, &mut |outcome| {
                let now = self.sim.now.get();
                if let Err(e) = &outcome { *self.errors.borrow_mut().entry(e.split(':').next().unwrap_or_default().to_string()).or_default() += 1 }
                if let Some((body, answered)) = &hook_in {
                    let status = match &outcome { Ok(v) => v["statusCode"].as_u64().unwrap_or(500) as u16, Err(_) => 502 };
                    let status = if now - started > GITHUB_WAITS_MS { 0 } else { status };
                    if status == 0 || status >= 500 { self.failed_deliveries.set(self.failed_deliveries.get() + 1) }
                    self.github.delivered(body, status, "", URL, now);
                    answered.set(true);
                }
                if let (Err(_), Some((payload, attempt))) = (&outcome, &retry_in) { if *attempt < 2 { self.at(now + 60_000, Event::Call(Call::Async(payload.clone(), attempt + 1))) } }
            }).await;
            drop(secrets);
            held.done = true;
        });
        Task { run, wake_at: started, deadline: started + LAMBDA_MS, done: false, kind, retry, hook }
    }

    /// An invocation Lambda ended at its time limit: its webhook unanswered fails at GitHub; an asynchronous one is
    /// called again.
    fn timed_out(&self, task: Task<'w>) {
        let now = self.sim.now.get();
        *self.timeouts.borrow_mut().entry(task.kind.clone()).or_default() += 1;
        let Task { run, retry, hook, .. } = task;
        drop(run);
        if let Some((body, answered)) = hook { if !answered.get() { self.failed_deliveries.set(self.failed_deliveries.get() + 1); self.github.delivered(&body, 0, "", URL, now) } }
        if let Some((payload, attempt)) = retry { if attempt < 2 { self.at(now + 60_000, Event::Call(Call::Async(payload, attempt + 1))) } }
    }
}

/// What a run came to.
#[derive(Debug, Default)]
struct Report {
    jobs: usize, ran: usize, waits_s: Vec<u64>, throttled: u64, hottest: Vec<(String, u64)>, throttled_in: BTreeMap<String, u64>, most_written: Vec<(String, f64)>, most_read: Vec<(String, f64)>,
    errors: BTreeMap<String, u64>, timeouts: BTreeMap<String, u64>, failed_deliveries: u64, cold_starts: u64, most_at_once: usize, lost_mid_job: usize, left_up: usize,
    busy_share: f64, filled: f64, minutes: u64,
}

impl Report {
    fn pct(&self, p: usize) -> u64 { let mut w = self.waits_s.clone(); w.sort_unstable(); if w.is_empty() { return u64::MAX } w[(w.len() * p / 100).min(w.len() - 1)] }
    fn said(&self) -> String {
        format!("{} of {} jobs ran in {} min; wait p50 {} s, p90 {} s, max {} s; DynamoDB throttled {} calls (by partition {:?}; items most throttled {:?}; most written {:?}; most read {:?}); \
            Lambda errors {:?}, timeouts {:?}; failed deliveries {}; {} cold starts, {} invocations at once at most; machines lost mid-job {}, left up {}; slots busy running jobs {:.0}%, slots held while jobs wait {:.0}%",
            self.ran, self.jobs, self.minutes, self.pct(50), self.pct(90), self.pct(100), self.throttled, self.throttled_in, self.hottest, self.most_written, self.most_read, self.errors, self.timeouts,
            self.failed_deliveries, self.cold_starts, self.most_at_once, self.lost_mid_job, self.left_up, self.busy_share * 100.0, self.filled * 100.0)
    }
}

/// `n` jobs queued over `spread_ms`, with `max_jobs` at once on the plane's one place (none: its default).
fn simulate(n: usize, spread_ms: u64, max_jobs: Option<u32>) -> Report {
    let sim = Sim { now: Cell::new(T0), due: Cell::new(u64::MAX) };
    let table = Table::default();
    // (As a plane in use has it: its layout this version's, its indexes built, its runs counted.)
    for (k, v) in [("layout", "spread".to_string()), ("indexed", T0.to_string()), ("runs-counted", T0.to_string())] {
        table.items.borrow_mut().insert((k.into(), k.into()), json!({ "p": { "S": k }, "k": { "S": k }, "v": { "S": v } }));
    }
    let github = superci_testkit::GitHub::default();
    let router = Router { sim: &sim, table: &table, github: &github, ssm: Cell::new((0, 0)), invoked: RefCell::new(vec![]) };
    let machines = Machines { sim: &sim, booting: RefCell::new(vec![]), stopped: RefCell::new(BTreeMap::new()), started: RefCell::new(BTreeSet::new()) };
    let configure = move |s: &Secrets, _: &str| {
        let mut c = Config::new(PLANE.into());
        c.read_settings(&s.values);
        c.containers = true;
        c.routing.daily_usd = Some(1_500.0);
        if let Some(m) = max_jobs { c.routing.order = vec![Pool { max_jobs: Some(m), ..Pool::of(&c.own_cloud) }] }
        c
    };
    let world = World { sim: &sim, router: &router, machines: &machines, github: &github, configure: &configure, idle: RefCell::new(vec![]), busy: Cell::new(0), most: Cell::new(0),
        cold: Cell::new(0), events: RefCell::new(BTreeMap::new()), errors: RefCell::new(BTreeMap::new()), timeouts: RefCell::new(BTreeMap::new()), failed_deliveries: Cell::new(0) };
    // The jobs, queued one after another over the spread; the schedule's sweep every minute.
    let mut queued: BTreeMap<(u64, u64), ()> = BTreeMap::new();
    let mut created: HashMap<u64, u64> = HashMap::new();
    for i in 0..n {
        let at = T0 + 5_000 + i as u64 * spread_ms / n.max(1) as u64;
        let (id, hook) = github.queue(&format!("matrix ({i})"), "superci", at);
        queued.insert((at, id), ());
        created.insert(id, at);
        world.at(at, Event::Call(Call::Hook(hook.0, hook.1)));
    }
    let end = T0 + 50 * 60_000;
    // (The first before the jobs, as on a plane in use: what it notes, such as the month's spend, is there.)
    for m in 0..50 { world.at(T0 + m * 60_000, Event::Call(Call::Async(json!({ "superci": "sweep" }), 0))) }
    let (mut tasks, mut started, mut finished, mut running): (Vec<Task<'_>>, HashMap<u64, u64>, BTreeSet<u64>, BTreeMap<String, (u64, u64, u64)>) = (vec![], HashMap::new(), BTreeSet::new(), BTreeMap::new());
    let (mut lost, mut busy_ms, mut last_end) = (0usize, 0u64, T0);
    // (Slot-milliseconds held by machines, and offered, while jobs wait at GitHub after the first half minute.)
    let slots = max_jobs.map_or(20, |m| m.min(n as u32)) as u64;
    let (mut held_ms, mut offered_ms) = (0u64, 0u64);
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        let now = sim.now.get();
        for p in router.invoked.take() { world.at(now, Event::Call(Call::Async(p, 0))) }
        for (body, signature) in github.to_send_again() { world.at(now, Event::Call(Call::Hook(body, signature))) }
        for (at, runner) in machines.booting.take() { world.at(at, Event::Boot(runner)) }
        let due: Vec<Event> = { let mut ev = world.events.borrow_mut(); let later = ev.split_off(&(now + 1)); std::mem::replace(&mut *ev, later).into_values().flatten().collect() };
        for e in due {
            match e {
                Event::Call(c) => tasks.push(world.invocation(c)),
                Event::Boot(runner) => {
                    if machines.stopped.borrow().contains_key(&runner) { continue }
                    let Some(((_, id), ())) = queued.pop_first() else { continue };
                    started.insert(id, now);
                    let ran_ms = 60_000 + fnv(&id.to_string()) % 60_000;
                    running.insert(runner.clone(), (id, now, now + ran_ms));
                    let hook = github.start_on(id, &runner);
                    tasks.push(world.invocation(Call::Hook(hook.0, hook.1)));
                    world.at(now + ran_ms, Event::Finish(id, runner));
                }
                Event::Finish(id, runner) => {
                    let (_, from, _) = running.remove(&runner).unwrap();
                    if machines.stopped.borrow().get(&runner).is_some_and(|at| *at < now) { lost += 1 }
                    busy_ms += now - from;
                    last_end = now;
                    finished.insert(id);
                    let hook = github.finish(id, "success", "ok", now);
                    tasks.push(world.invocation(Call::Hook(hook.0, hook.1)));
                }
            }
        }
        let mut polled = false;
        for t in tasks.iter_mut().filter(|t| t.wake_at <= now) {
            polled = true;
            sim.due.set(u64::MAX);
            if t.run.as_mut().poll(&mut cx).is_ready() { t.done = true } else {
                assert!(sim.due.get() != u64::MAX, "an invocation waits on something other than time");
                t.wake_at = sim.due.get();
            }
        }
        tasks.retain(|t| !t.done);
        if polled || !router.invoked.borrow().is_empty() || !machines.booting.borrow().is_empty() { continue }
        let settled = finished.len() == n && now > last_end + 6 * 60_000;
        let next = tasks.iter().map(|t| t.wake_at.min(t.deadline)).chain(world.events.borrow().keys().next().copied()).min().unwrap_or(u64::MAX);
        if settled || next >= end { break }
        if !queued.is_empty() && now > T0 + 35_000 {
            let up = machines.started.borrow().len() - machines.stopped.borrow().len();
            held_ms += (up as u64).min(slots) * (next - now);
            offered_ms += slots * (next - now);
        }
        sim.now.set(next);
        let (over, kept): (Vec<Task<'_>>, Vec<Task<'_>>) = tasks.into_iter().partition(|t| t.deadline <= next);
        tasks = kept;
        for t in over { world.timed_out(t) }
    }
    let up = machines.started.borrow().iter().filter(|m| !machines.stopped.borrow().contains_key(*m)).count();
    let mut hottest: Vec<(String, u64)> = table.throttled.borrow().iter().map(|(k, v)| (k.clone(), *v)).collect();
    hottest.sort_by(|a, b| b.1.cmp(&a.1));
    hottest.truncate(6);
    let mut most_written: Vec<(String, f64)> = table.wrote.borrow().iter().map(|(k, v)| (k.clone(), *v)).collect();
    most_written.sort_by(|a, b| b.1.total_cmp(&a.1));
    most_written.truncate(4);
    let mut most_read: Vec<(String, f64)> = table.read.borrow().iter().map(|(k, v)| (k.clone(), *v)).collect();
    most_read.sort_by(|a, b| b.1.total_cmp(&a.1));
    most_read.truncate(6);
    let slots = slots as f64;
    let span = last_end.saturating_sub(T0 + 5_000).max(1) as f64;
    let report = Report {
        jobs: n, ran: started.len(), waits_s: started.iter().map(|(id, at)| (at - created[id]) / 1000).collect(), throttled: table.throttled.borrow().values().sum(), hottest,
        throttled_in: table.throttled_in.borrow().clone(), most_written, most_read, errors: world.errors.borrow().clone(), timeouts: world.timeouts.borrow().clone(),
        failed_deliveries: world.failed_deliveries.get(), cold_starts: world.cold.get(), most_at_once: world.most.get(), lost_mid_job: lost, left_up: up,
        busy_share: busy_ms as f64 / (slots * span), filled: held_ms as f64 / offered_ms.max(1) as f64, minutes: (sim.now.get() - T0) / 60_000,
    };
    report
}

/// What every burst must come to, whatever the limits: every job runs, nothing is throttled, no invocation fails or
/// runs out of time, GitHub has every answer, no machine ends under its job and none is left up.
fn sound(r: &Report) -> Vec<String> {
    let mut wrong = vec![];
    if r.ran < r.jobs { wrong.push(format!("{} jobs never ran", r.jobs - r.ran)) }
    if r.throttled > 0 { wrong.push(format!("DynamoDB throttled {} calls", r.throttled)) }
    if !r.errors.is_empty() { wrong.push(format!("Lambda errors {:?}", r.errors)) }
    if !r.timeouts.is_empty() { wrong.push(format!("Lambda timeouts {:?}", r.timeouts)) }
    if r.failed_deliveries > 0 { wrong.push(format!("{} failed deliveries", r.failed_deliveries)) }
    if r.lost_mid_job > 0 { wrong.push(format!("{} machines ended under their job", r.lost_mid_job)) }
    if r.left_up > 0 { wrong.push(format!("{} machines left up", r.left_up)) }
    wrong
}

/// A thousand jobs queued in ten seconds, with room for all of them: each starts within a minute of being queued.
#[test]
fn a_thousand_jobs_queued_at_once_each_start_within_a_minute() {
    let r = simulate(1_000, 10_000, Some(5_000));
    eprintln!("burst: {}", r.said());
    let mut wrong = sound(&r);
    if r.pct(90) >= 60 { wrong.push(format!("wait p90 {} s", r.pct(90))) }
    assert!(wrong.is_empty(), "{}\n{}", wrong.join("; "), r.said());
}

/// Three hundred jobs queued at once on a place that runs 20 at once: as each ends, a waiting one gets its machine, so
/// while jobs wait the place's 20 slots stay held.
#[test]
fn jobs_past_a_places_limit_start_as_room_frees() {
    let r = simulate(300, 10_000, None);
    eprintln!("limit: {}", r.said());
    let mut wrong = sound(&r);
    if r.filled < 0.97 { wrong.push(format!("slots held {:.1}% of the time jobs waited", r.filled * 100.0)) }
    assert!(wrong.is_empty(), "{}\n{}", wrong.join("; "), r.said());
}
