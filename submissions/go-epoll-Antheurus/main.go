package main

import (
	"log"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
)

// workers is how many processes share the listening port through SO_REUSEPORT. One core does
// not need two event loops for CPU; it needs two file-descriptor tables. LimitNOFILE=65535 is
// per process, so one process can hold ~59k sockets and two hold ~118k, which keeps every k6
// user's keep-alive connection open instead of closing and re-accepting them. Each extra process
// costs CPU even when idle-ish (more wakeups, smaller batches): at 80k users two processes
// measured 53-55% CPU and three 68%, so this is the fewest that hold the connections.
const workers = 2

const workerEnv = "TQ_WORKER"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	path, secret := mustEnv("SQLITE_PATH"), mustEnv("JWT_SECRET")
	host := envOr("HOST", "127.0.0.1")
	port, err := strconv.Atoi(envOr("PORT", "3000"))
	if err != nil {
		log.Fatalf("PORT: %v", err)
	}
	worker := os.Getenv(workerEnv)
	log.SetPrefix("[w" + envOr(workerEnv, "0") + "] ")

	// Steady state allocates nothing, so the collector only runs at startup; the limit is a
	// backstop that keeps the heap well inside the 2 GB box if that ever stops being true.
	debug.SetGCPercent(400)
	debug.SetMemoryLimit(384 << 20)

	d, err := openDB(path, []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA wal_autocheckpoint = 0",
		"PRAGMA mmap_size = 1073741824",
		"PRAGMA cache_size = -16384",
		"PRAGMA temp_store = MEMORY",
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := d.prepareAll(); err != nil {
		log.Fatal(err)
	}

	runtime.LockOSThread()
	s, err := newServer(host, port, newApp(d, secret))
	if err != nil {
		log.Fatal(err)
	}
	if worker == "" {
		go checkpointer(path)
		for i := 1; i < workers; i++ {
			go superviseWorker(i)
		}
	}
	s.run()
}

// superviseWorker runs one extra worker process and restarts it if it ever exits. The worker
// dies with this process (Pdeathsig), so the service stays a single foreground process tree.
func superviseWorker(i int) {
	// Pdeathsig fires when the forking THREAD exits; pin this goroutine so that thread lives as
	// long as the process does.
	runtime.LockOSThread()
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("worker %d: %v", i, err)
	}
	for {
		cmd := exec.Command(self)
		cmd.Env = append(os.Environ(), workerEnv+"="+strconv.Itoa(i))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		err := cmd.Run()
		log.Printf("worker %d exited (%v), restarting", i, err)
		time.Sleep(200 * time.Millisecond)
	}
}

// checkpointer moves WAL frames into the database file on its own connection and thread, so
// the fsync a checkpoint needs never stalls an event loop (they run with autocheckpoint off).
func checkpointer(path string) {
	cp, err := openDB(path, []string{"PRAGMA synchronous = NORMAL", "PRAGMA busy_timeout = 2000"})
	if err != nil {
		log.Fatalf("checkpointer: %v", err)
	}
	for range time.Tick(time.Second) {
		cp.checkpoint()
	}
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
