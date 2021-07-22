package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/diamondburned/arikawa/v2/api"
	"github.com/diamondburned/arikawa/v2/discord"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

func main() {
	token := os.Getenv("TOKEN")
	if token == "" {
		log.Fatalln("missing $TOKEN")
	}

	flag.Parse()

	dest := flag.Arg(0)
	if dest == "" {
		log.Fatalln("missing argument; usage: discord-emoji-dl <dest folder>")
	}

	if err := os.MkdirAll(dest, os.ModePerm); err != nil {
		log.Fatalln("failed to mkdir -p dest:", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := api.NewClient(token)

	guilds, err := client.Guilds(0)
	if err != nil {
		log.Fatalln("failed to get guilds:", err)
	}

	workers := WorkerManager{Destination: dest}
	input := workers.Start(ctx, runtime.GOMAXPROCS(-1)*2)

	for _, guild := range guilds {
		emojis, err := client.Emojis(guild.ID)
		if err != nil {
			log.Printf("failed to get emojis for guild %s: %v", guild.Name, err)
			continue
		}

		for _, emoji := range emojis {
			job := EmojiJob{
				Emoji: emoji.Name,
				Guild: guild.Name,
				URL:   emoji.EmojiURLWithType(discord.PNGImage),
			}

			select {
			case input <- job:
				continue
			case <-ctx.Done():
				log.Fatalln("job cancelled")
			}
		}
	}

	if err := workers.Wait(); err != nil {
		log.Fatalln("emoji workers failed:", err)
	}
}

type EmojiJob struct {
	Emoji string
	Guild string
	URL   string
}

type WorkerManager struct {
	// Destination is the directory to download to.
	Destination string

	dir sync.Map
	dmu sync.Mutex

	in   chan EmojiJob
	errg errgroup.Group
}

// Start starts up n workers that are stopped once ctx is cancelled. It returns
// the job input channel.
func (m *WorkerManager) Start(ctx context.Context, n int) chan<- EmojiJob {
	if m.in != nil {
		return m.in
	}

	m.in = make(chan EmojiJob)

	for i := 0; i < n; i++ {
		m.errg.Go(m.work)
	}

	return m.in
}

// Wait signals that all jobs are sent and blocks until the workers exit.
// Sending into the returned Input channel will panic. The worker manager cannot
// be used once Wait is called.
func (m *WorkerManager) Wait() error {
	if m.in == nil {
		return errors.New("Wait called on unstarted manager")
	}

	close(m.in)
	return m.errg.Wait()
}

func (m *WorkerManager) work() error {
	buf := make([]byte, 102400) // 100KB

	for job := range m.in {
		if err := m.do(job, buf); err != nil {
			return err
		}
	}

	return nil
}

func (m *WorkerManager) do(job EmojiJob, buf []byte) error {
	dirPath := filepath.Join(m.Destination, job.Guild)

	// If the guild directory isn't made yet, then do so.
	if _, ok := m.dir.Load(job.Guild); !ok {
		m.dmu.Lock()
		if err := os.MkdirAll(dirPath, os.ModePerm); err != nil {
			log.Println("failed to mkdir -p:", err)
			return errors.Wrap(err, "worker fail: failed to mkdir -p")
		}
		m.dir.Store(job.Guild, struct{}{})
		m.dmu.Unlock()
	}

	dstPath := filepath.Join(dirPath, job.Emoji+".png")

	r, err := http.Get(job.URL)
	if err != nil {
		log.Println("failed to GET emoji:", err)
		return nil
	}
	defer r.Body.Close()

	f, err := os.Create(dstPath)
	if err != nil {
		log.Println("failed to touch:", err)
		return errors.Wrap(err, "worker fail: failed to touch")
	}
	defer f.Close()

	if _, err := io.CopyBuffer(f, r.Body, buf); err != nil {
		log.Println("failed to download:", err)
		return nil
	}

	return nil
}
