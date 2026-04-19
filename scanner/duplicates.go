package scanner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"

	"GoPurge/model"
)

const headerSize = 1024 // bytes read for Stage B header sampling

// hashResult carries the outcome of hashing a single file in the worker pool.
type hashResult struct {
	entry model.FileEntry
	err   error
}

// groupBySize partitions the asset list into buckets keyed by file size.
// Buckets with only one entry are discarded — a file with a unique size
// cannot have a duplicate.
func groupBySize(assets []model.FileEntry) map[int64][]model.FileEntry {
	// Stage A: group by size. This is a single-pass O(n) operation.
	bySize := make(map[int64][]model.FileEntry)
	// Iterate over all assets and append them to the bySize map under their Size key.
	for _, asset := range assets {
		bySize[asset.Size] = append(bySize[asset.Size], asset)
	}
	// Filter out size groups that have only one file, as they cannot be duplicates.
	candidates := make(map[int64][]model.FileEntry)
	for size, group := range bySize {
		if len(group) > 1 {
			candidates[size] = group
		}
	}
	return candidates
}

// groupByHeader reads the first headerSize bytes of each file in the candidate
// map and further partitions them by header content. Entries with a unique
// header within their size bucket are dropped. Non-fatal read errors are
// appended to warnings and the affected entry is skipped.
func groupByHeader(candidates map[int64][]model.FileEntry, warnings *[]string) [][]model.FileEntry {
	// Stage B: group by header. This is also O(n) but involves disk I/O,
	// so we log warnings for any read errors instead of aborting.
	var result [][]model.FileEntry

	// Iterate over each size group and read the header of each file.
	// Group files by their header content using a map where the key is the header
	// and the value is a slice of FileEntry that share that header.
	for _, group := range candidates {
		groupsByHeader := make(map[string][]model.FileEntry)
		for _, entry := range group {
			header, err := readHeader(entry.Path)
			if err != nil {
				*warnings = append(*warnings, fmt.Sprintf("header read skipped %q: %v", entry.Path, err))
				continue
			}
			groupsByHeader[header] = append(groupsByHeader[header], entry)
		}

		// After processing each size group, we append only those header groups with
		// more than one file to the result, as they are potential duplicates.
		for _, subGroup := range groupsByHeader {
			if len(subGroup) > 1 {
				result = append(result, subGroup)
			}
		}
	}
	return result
}

// readHeader reads up to headerSize bytes from the file at path and returns
// them as a hex string suitable for use as a map key.
func readHeader(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Use io.ReadFull to read exactly headerSize bytes, or until EOF if the file is smaller.
	// This ensures we get a consistent header for files smaller than headerSize.
	buf := make([]byte, headerSize)
	numOfBytes, err := io.ReadFull(file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", err
	}

	// Encode the read bytes to a hex string to use as a map key.
	return hex.EncodeToString(buf[:numOfBytes]), nil
}

// hashCandidates computes the full SHA-256 of each file in the candidate groups
// using a fan-out/fan-in worker pool. Files that produce the same digest are
// returned as a model.FileGroup. Non-fatal errors are appended to warnings.
func hashCandidates(candidates [][]model.FileEntry, workers int, warnings *[]string) ([]model.FileGroup, error) {
	// Flatten all candidates into a single job stream.
	var allCandidates []model.FileEntry

	// Iterate over the candidate groups (which are grouped by size and header) and
	// append all FileEntry objects to the allCandidates slice, which will be fed into the worker pool for hashing.
	// ... means to "unpack" the slice of FileEntry from each group and append them individually to allCandidates.
	for _, group := range candidates {
		allCandidates = append(allCandidates, group...)
	}

	// Set up channels for the worker pool: jobs for input and results for output.
	// The jobs channel is buffered (size workers) to allow workers to pull tasks without blocking the main goroutine.
	// The results channel is also buffered (size workers) to allow workers to send results without blocking.
	jobs := make(chan model.FileEntry, workers)
	results := make(chan hashResult, workers)

	// Fan-out: spawn N worker goroutines.
	var waitGroup sync.WaitGroup
	for workerIndex := 0; workerIndex < workers; workerIndex++ {
		waitGroup.Add(1)
		// Each worker reads from the jobs channel, processes the file by hashing it, and sends the result to the results channel.
		go func() {
			defer waitGroup.Done()
			for entry := range jobs {
				// For each FileEntry received from the jobs channel, we attempt to hash the file at entry.Path.
				// If hashing is successful, we send a hashResult with the entry (including its SHA256 field) to the results channel.
				// If an error occurs during hashing, we send a hashResult with the error and log a warning instead of aborting.
				digest, err := hashFile(entry.Path)
				if err != nil {
					results <- hashResult{err: fmt.Errorf("%q: %w", entry.Path, err)}
					continue
				}
				entry.SHA256 = digest
				results <- hashResult{entry: entry}
			}
		}()
	}

	// Close results once all workers finish.
	go func() {
		waitGroup.Wait()
		close(results)
	}()

	// 1. The Workers Start First (But Wait)
	//    Between duplicates.go:118-132, we spawn the worker goroutines.
	//    These workers immediately run for entry := range jobs.
	//    Because the jobs channel is empty at this moment, all N workers pause (block) right there.
	//    They are not consuming CPU; they are simply waiting for something to arrive in the channel.

	// 2. The Feeder Starts (The "Unlock")
	//    At duplicates.go:162-167, we start the "feeder" goroutine.
	//    As soon as this goroutine puts the first FileEntry into jobs,
	//    one of the waiting workers wakes up instantly, takes the entry, and starts hashing.
	//    This continues until all files are fed and the channel is closed.

	// 3. Why we don't do it sequentially
	//    If you tried to feed the channel before starting the workers
	//    The program would crash (deadlock). Since the channel has a limited capacity (workers),
	//    the feeder would fill it up and then pause, waiting for someone to take an item.
	//    If no workers have been started yet, no one will ever take an item, and the program will hang forever.
	go func() {
		for _, entry := range allCandidates {
			jobs <- entry
		}
		close(jobs)
	}()

	// Fan-in: collect results and group by SHA-256.
	// As workers finish hashing files, they send hashResult objects to the results channel.
	// We read from this channel until it's closed (which happens after all workers are done).
	// For each hashResult, if there is an error, we log a warning.
	// If there is no error, we group the FileEntry by its SHA256 digest in the groupByHash map.
	groupByHash := make(map[string][]model.FileEntry)
	for result := range results {
		if result.err != nil {
			*warnings = append(*warnings, fmt.Sprintf("hash skipped: %v", result.err))
			continue
		}
		groupByHash[result.entry.SHA256] = append(groupByHash[result.entry.SHA256], result.entry)
	}

	// Return only groups with two or more confirmed duplicates.
	var groups []model.FileGroup
	for hash, files := range groupByHash {
		if len(files) > 1 {
			groups = append(groups, model.FileGroup{Hash: hash, Files: files})
		}
	}
	return groups, nil
}

// hashFile streams the file at path through SHA-256 using io.Copy to keep
// memory usage bounded regardless of file size.

// Unreal Engine assets (.uasset, .umap) can be massive—often hundreds of megabytes or even gigabytes.
//
// If we used a "helper" like sha256.Sum(data):
// We would first have to read the entire file into a byte slice (os.ReadFile).
// If we have 4 workers hashing 2GB files simultaneously, our app would suddenly demand 8GB of RAM and likely crash.
//
// By using io.Copy(h, f): We are "streaming" the file.
//    Go reads a small chunk (usually 32KB) into a tiny buffer, passes it to the hash function, and
//    then reuses that same tiny buffer for the next chunk.
//    This keeps our RAM usage at ~50MB regardless of whether the files being hashed are 1KB or 10GB.
//
// If we look at the source code for io.Copy, it does something like this behind the scenes:
// Buffer Allocation: 
//    It creates a small, fixed-size internal buffer (usually 32 KB).
// The Loop:
//    It reads 32 KB from the source (os.File).
//    It writes that 32 KB to the destination (sha256.New()).
//    It repeats until the end of the file is reached.
//    The crucial part is that io.Copy never holds the entire file in memory at once.
func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Create a new SHA-256 hash.Hash object and copy the file contents into it.
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
