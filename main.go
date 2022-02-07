package main

import (
	"context"
	"github.com/PuerkitoBio/goquery"
	"go.uber.org/zap"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	minLvl = 0
	logger *zap.Logger
)

type CrawlResult struct {
	Err   error
	Title string
	Url   string
}

type Page interface {
	GetTitle() string
	GetLinks() []string
}

type page struct {
	doc *goquery.Document
}

func NewPage(raw io.Reader) (Page, error) {
	doc, err := goquery.NewDocumentFromReader(raw)
	if err != nil {
		return nil, err
	}
	return &page{doc: doc}, nil
}

func (p *page) GetTitle() string {
	return p.doc.Find("title").First().Text()
}

func (p *page) GetLinks() []string {
	var urls []string
	p.doc.Find("a").Each(func(_ int, s *goquery.Selection) {
		url, ok := s.Attr("href")
		if ok {
			urls = append(urls, url)
		}
	})
	return urls
}

type Requester interface {
	Get(ctx context.Context, url string) (Page, error)
}

type requester struct {
	timeout time.Duration
}

func NewRequester(timeout time.Duration) requester {
	return requester{timeout: timeout}
}

func (r requester) Get(ctx context.Context, url string) (Page, error) {
	select {
	case <-ctx.Done():
		return nil, nil
	default:
		cl := &http.Client{
			Timeout: r.timeout,
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		body, err := cl.Do(req)
		if err != nil {
			return nil, err
		}
		defer body.Body.Close()
		page, err := NewPage(body.Body)
		if err != nil {
			return nil, err
		}
		return page, nil
	}
	return nil, nil
}

//Crawler - интерфейс (контракт) краулера
type Crawler interface {
	Scan(ctx context.Context, url string, lvl int)
	ChanResult() <-chan CrawlResult
	ProcessResult(ctx context.Context, cancel func())
}

type crawler struct {
	r       Requester
	res     chan CrawlResult
	visited map[string]struct{}
	mu      sync.RWMutex
	cfg     Config
	Lvl     int
}

func NewCrawler(r Requester, cfg Config) *crawler {
	return &crawler{
		r:       r,
		res:     make(chan CrawlResult),
		visited: make(map[string]struct{}),
		cfg:     cfg,
		mu:      sync.RWMutex{},
		Lvl:     0,
	}
}

func (c *crawler) Scan(ctx context.Context, url string, lvl int) {

	if lvl <= minLvl { //Проверяем то, что есть запас по глубине
		return
	}
	c.mu.RLock()
	_, ok := c.visited[url] //Проверяем, что мы ещё не смотрели эту страницу
	c.mu.RUnlock()
	if ok {
		return
	}
	select {
	case <-ctx.Done(): //Если контекст завершен - прекращаем выполнение
		return
	default:
		page, err := c.r.Get(ctx, url) //Запрашиваем страницу через Requester
		if err != nil {
			c.res <- CrawlResult{Err: err} //Записываем ошибку в канал
			return
		}
		c.mu.Lock()
		c.visited[url] = struct{}{} //Помечаем страницу просмотренной
		c.mu.Unlock()
		c.res <- CrawlResult{ //Отправляем результаты в канал
			Title: page.GetTitle(),
			Url:   url,
		}

		for _, link := range page.GetLinks() {
			go c.Scan(ctx, link, lvl-1) //На все полученные ссылки запускаем новую рутину сборки
		}
	}
}

func (c *crawler) ChanResult() <-chan CrawlResult {
	return c.res
}

//Config - структура для конфигурации
type Config struct {
	MaxDepth   int
	MaxResults int
	MaxErrors  int
	Url        string
	Timeout    int //in seconds
}

func main() {
	logger, _ = zap.NewProduction()

	defer logger.Sync()





	//logger.With(zap.Uint64("uid", 101345)).Error("file corrupted")
	//logger.Info("storage space left: " + strconv.Itoa(1024))
	cfg := Config{
		MaxDepth:   3,
		MaxResults: 10,
		MaxErrors:  10,
		Url:        "https://www.rbc.ru/",
		Timeout:    10,
	}

	var cr Crawler
	var r Requester

	r = NewRequester(time.Duration(cfg.Timeout) * time.Second)
	cr = NewCrawler(r, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	go cr.Scan(ctx, cfg.Url, cfg.MaxDepth) //Запускаем краулер в отдельной рутине
	logger.Info("Start crawler")
	go cr.ProcessResult(ctx, cancel)       //Обрабатываем результаты в отдельной рутине
	logger.Info("Process Results")
	sigCh := make(chan os.Signal) //Создаем канал для приема сигналов
	logger.Info("Make channel for signal")
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGUSR1) //Подписываемся на сигнал SIGINT SIGUSR1
	for {
		select {
		case <-ctx.Done(): //Если всё завершили - выходим
			return

		case sig := <-sigCh:
			if sig == syscall.SIGINT {
				cancel() //Если пришёл сигнал SigInt - завершаем контекст
			}
			log.Println(sig)
			if sig == syscall.SIGUSR1 {
				minLvl -= 2
				logger.Panic("died")
			}
		}
	}
}

func (c *crawler) ProcessResult(ctx context.Context, cancel func()) {
	var maxResult, maxErrors = c.cfg.MaxResults, c.cfg.MaxErrors
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.ChanResult():
			//time.Sleep(5 * time.Second)
			//fmt.Println(c.cfg, minLvl)
			if msg.Err != nil {
				maxErrors--
				logger.Warn(msg.Err.Error())
				if maxErrors <= 0 {
					cancel()
					return
				}
			} else {
				maxResult--
				logger.With(zap.String("url",msg.Url), zap.String("title",msg.Title)).Info(" successful")
				if maxResult <= 0 {
					cancel()
					return
				}
			}
		}
	}
}
