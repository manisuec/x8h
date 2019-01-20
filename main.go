package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	topStories           = "https://hacker-news.firebaseio.com/v0/topstories.json"
	bestStories          = "https://hacker-news.firebaseio.com/v0/beststories.json"
	storyLink            = "https://hacker-news.firebaseio.com/v0/item/%d.json"
	hnPostLink           = "https://news.ycombinator.com/item?id=%d"
	frontPageNumArticles = 30
	hnPollTime           = 1 * time.Minute
	defaultPort          = 8080
)

type limitMap struct {
	sync.Mutex
	keys []int
	m    map[int]struct{}
	l    int
}

func (lm *limitMap) insert(k int) {
	lm.Lock()
	defer lm.Unlock()

	lm.m[k] = struct{}{}
	lm.keys = append(lm.keys, k)

	if len(lm.keys) >= lm.l {
		delete(lm.m, lm.keys[0])
		lm.keys = lm.keys[1:]
	}
}

func (lm *limitMap) has(k int) bool {
	lm.Lock()
	defer lm.Unlock()

	_, ok := lm.m[k]

	return ok
}

func newLM(m map[int]struct{}, l int) *limitMap {
	keys := []int{}
	for k, _ := range m {
		keys = append(keys, k)
	}
	return &limitMap{
		keys: keys,
		m:    m,
		l:    l,
	}
}

type itemList []int

type unixTime int64

type item struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Deleted bool   `json:"deleted"`
	Dead    bool   `json:"dead"`

	Added unixTime `json:"-"`
}

type stories struct {
	sync.Mutex
	list map[int]item
}

func main() {
	var port int
	// use PORT env else port
	envPort := os.Getenv("PORT")
	if envPort == "" {
		port = defaultPort
	} else {
		var err error
		port, err = strconv.Atoi(envPort)
		if err != nil {
			panic(err)
		}
	}

	tmpl := template.New("index.html")
	tmpl, err := tmpl.ParseFiles("./index.html")
	if err != nil {
		panic(err)
	}

	errCh := make(chan error)

	st := stories{
		list: make(map[int]item),
	}

	storiesURLs := []string{topStories}
	incomingItems := make(chan itemList)

	const trackingLimit = 2000
	visited := make(map[int]struct{})
	lm := newLM(visited, trackingLimit)

	topStoriesFetcher := func(ctx context.Context, limit int) error {
		eg, ctx := errgroup.WithContext(ctx)
		for _, storiesURL := range storiesURLs {
			storiesURL := storiesURL
			eg.Go(func() error {
				fetchStories := func(limit int) ([]int, error) {
					resp, err := http.Get(storiesURL)
					if err != nil {
						return nil, err
					}

					defer resp.Body.Close()

					decoder := json.NewDecoder(resp.Body)
					var items itemList
					err = decoder.Decode(&items)
					if err != nil {
						return nil, err
					}
					if len(items) < limit {
						limit = len(items)
					}
					return items[:limit], nil
				}

				// send items
				items, err := fetchStories(limit)
				if err != nil {
					return err
				}
				incomingItems <- items
				return nil
			})
		}

		return eg.Wait()
	}

	fetchItem := func(itemID int) (i item, err error) {
		resp, err := http.Get(fmt.Sprintf(storyLink, itemID))
		if err != nil {
			return
		}

		defer resp.Body.Close()

		decoder := json.NewDecoder(resp.Body)
		err = decoder.Decode(&i)
		if err != nil {
			return
		}

		return i, nil
	}

	storyLister := func(ctx context.Context) error {
		for {
			select {
			case items := <-incomingItems:
				for _, itemID := range items {
					select {
					case <-ctx.Done():
						return nil
					default:
						func() {
							if _, ok := st.list[itemID]; ok {
								return
							}
							if lm.has(itemID) {
								return
							}
							item, err := fetchItem(itemID)
							if err != nil {
								errCh <- err
								return
							}

							lm.insert(itemID)
							item.Added = unixTime(time.Now().Unix())

							st.Lock()
							defer st.Unlock()
							st.list[itemID] = item
						}()
					}
				}
			case <-ctx.Done():
				return nil
			}
		}
	}

	storyRemover := func(ctx context.Context) error {
		st.Lock()
		defer st.Unlock()
		for id, it := range st.list {
			if ctx.Err() != nil {
				return nil
			}
			if time.Since(time.Unix(int64(it.Added), 0)).Seconds() > 8*60*60 {
				delete(st.list, id)
			}
		}
		return nil
	}

	listCounter := func() error {
		st.Lock()
		defer st.Unlock()
		var ids []int
		for id := range st.list {
			ids = append(ids, id)
		}
		log.Println(ids)
		ids = ids[:0]
		for id := range visited {
			ids = append(ids, id)
		}
		log.Println(ids)
		return nil
	}

	errLogger := func() error {
		for err := range errCh {
			if err != nil {
				log.Println(err)
			}
		}
		return nil
	}

	visitCounterCh := make(chan int)

	var visitCount int
	visitCounter := func() error {
		for c := range visitCounterCh {
			visitCount += c
		}
		return nil
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			visitCounterCh <- 1
		}()

		st.Lock()
		defer st.Unlock()

		data := make(map[string]interface{})
		data["Data"] = st.list
		data["VisitorNumber"] = visitCount

		err = tmpl.Execute(w, data)
		if err != nil {
			errCh <- err
		}
		return
	})

	log.Println("START")
	log.Println("starting the app")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, os.Kill)

	appCtx, cancel := context.WithCancel(context.Background())

	fiveMinTicker := time.NewTicker(hnPollTime)

	go func() {
		for range fiveMinTicker.C {
			log.Println("starting ticker ticker")
			eg, ctx := errgroup.WithContext(appCtx)
			eg.Go(func() error {
				log.Println("starting top stories fetcher")
				return topStoriesFetcher(ctx, frontPageNumArticles)
			})
			eg.Go(func() error {
				log.Println("starting story remover")
				return storyRemover(ctx)
			})
			eg.Go(func() error {
				log.Println("starting list counter")
				return listCounter()
			})
			err := eg.Wait()
			if err != nil {
				errCh <- err
			}
		}
	}()

	eg, ctx := errgroup.WithContext(appCtx)

	eg.Go(func() error {
		log.Println("starting error logger")
		return errLogger()
	})
	eg.Go(func() error {
		log.Println("starting top stories fetcher")
		return topStoriesFetcher(ctx, frontPageNumArticles)
	})
	eg.Go(func() error {
		log.Println("starting story lister")
		return storyLister(ctx)
	})

	eg.Go(visitCounter)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port)}

	errors := make(chan error)
	defer close(errors)

	go func() {
		log.Println("starting http server on port 8080")
		errors <- srv.ListenAndServe()
	}()

	go func() {
		sig := <-stop
		errors <- fmt.Errorf("interrupted with signal %s, aborting", sig.String())
	}()

	go func() {
		errors <- eg.Wait()
	}()

	err = <-errors
	log.Println(err)

	// drain errors
	go func() {
		for err := range errors {
			log.Println(err)
		}
	}()

	err = srv.Shutdown(ctx)
	log.Println(err)

	cleanup := func() {
		cancel()
		fiveMinTicker.Stop()
		close(incomingItems)
		close(visitCounterCh)
		close(errCh)
	}

	cleanup()

	log.Println("END")
}
