# How go func() and channels work in the analyzer

**`go func()` does not block and does not wait for the previous one to finish**. Every `go` keyword launches a goroutine and immediately returns — the calling code moves on to the next line while the goroutine runs independently in the background. They all run simultaneously (or as simultaneously as the Go scheduler and your CPU cores allow).

Here is the exact execution order in `scanAssetBinaries`:

---

### Step 1 — Channels are created (sequential, instant)
```go
jobs    := make(chan model.FileEntry, workers)
results := make(chan scanResult, workers)
```
Two buffered channels exist. Nothing is in them yet. No goroutine is running.

---

### Step 2 — N worker goroutines are spawned (sequential loop, instant per iteration)
```go
for workerIndex := 0; workerIndex < workers; workerIndex++ {
    wg.Add(1)
    go func() { ... }()  // ← returns immediately
}
```
The `go` keyword schedules a goroutine and returns instantly. The loop completes at full CPU speed. All N workers now exist, but they are all immediately **blocked** trying to read from `jobs` (range jobs), which is empty. They are parked — consuming no CPU until work arrives.

---

### Step 3 — wg-closer goroutine is spawned (instant)
```go
go func() {
    wg.Wait()      // blocks here until all workers call wg.Done()
    close(results)
}()
```
Spawned and immediately blocked on `wg.Wait()`. It will only unblock once every worker finishes.

---

### Step 4 — Producer goroutine is spawned (instant)
```go
go func() {
    for _, asset := range assets { jobs <- asset }
    close(jobs)
}()
```
Spawned and starts sending assets into `jobs`. As soon as the first asset lands in the channel, a waiting worker goroutine wakes up and begins reading the file.

---

### Step 5 — Collector loop runs on the main goroutine (blocking)
```go
for result := range results { ... }
```
This is the **only line in the function that blocks the main goroutine**. It sits here reading results as workers produce them.

---

### The full picture while the collector waits

```
Main goroutine (collector)     blocked on:  for result := range results
Producer goroutine             sending into: jobs channel → then exits
wg-closer goroutine            blocked on:  wg.Wait()
Worker 0                       reading file, sending to results, then picking next job
Worker 1                       reading file, sending to results, then picking next job
Worker N-1                     ...
```

All of these are running at the same time. When the producer closes `jobs`, workers finish their last job, call `wg.Done()`, and exit. Once all workers are done `wg.Wait()` unblocks, the wg-closer calls `close(results)`, and the collector's `range` loop ends — `scanAssetBinaries` returns.

---

### Why does order matter at all then?

It doesn't for correctness — because the collector is the only writer to `referenced` and `warnings`, it processes results one at a time in whatever order they arrive. A file that finishes scanning first simply appears in results first. The final map is identical regardless of arrival order.