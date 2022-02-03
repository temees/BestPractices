package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
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
		Lvl: 0,
	}
}

func (c *crawler) Scan(ctx context.Context, url string, lvl int) {

	if lvl <= c.Lvl { //Проверяем то, что есть запас по глубине
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
			go c.Scan(ctx, link, lvl - 1) //На все полученные ссылки запускаем новую рутину сборки
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

	cfg := Config{
		MaxDepth:   3,
		MaxResults: 100,
		MaxErrors:  100,
		Url:        "https://www.rbc.ru/",
		Timeout:    10,
	}

	var cr Crawler
	var r Requester

	r = NewRequester(time.Duration(cfg.Timeout) * time.Second)
	cr = NewCrawler(r, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	go cr.Scan(ctx, cfg.Url, cfg.MaxDepth)         //Запускаем краулер в отдельной рутине
	go cr.ProcessResult(ctx, cancel) //Обрабатываем результаты в отдельной рутине

	sigCh := make(chan os.Signal)                         //Создаем канал для приема сигналов
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGUSR1) //Подписываемся на сигнал SIGINT SIGUSR1
	for {
		select {
		case <-ctx.Done(): //Если всё завершили - выходим
			return
		case sig := <-sigCh:
			if sig == syscall.SIGINT {
				cancel() //Если пришёл сигнал SigInt - завершаем контекст
			}
			fmt.Println(sig)
			if sig == syscall.SIGUSR1 {
				cr.Lvl -= 2
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
			fmt.Println(c.cfg)
			if msg.Err != nil {
				maxErrors--
				log.Printf("crawler result return err: %s\n", msg.Err.Error())
				if maxErrors <= 0 {
					cancel()
					return
				}
			} else {
				maxResult--
				log.Printf("crawler result: [url: %s] Title: %s\n", msg.Url, msg.Title)
				if maxResult <= 0 {
					cancel()
					return
				}
			}
		}
	}
}
