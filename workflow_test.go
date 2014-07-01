package main_test

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/alphagov/govuk_crawler_worker"

	. "github.com/alphagov/govuk_crawler_worker/http_crawler"
	. "github.com/alphagov/govuk_crawler_worker/queue"
	. "github.com/alphagov/govuk_crawler_worker/ttl_hash_set"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"os"

	"github.com/alphagov/govuk_crawler_worker/util"
	"github.com/fzzy/radix/redis"
	"github.com/streadway/amqp"
)

var _ = Describe("Workflow", func() {
	Describe("Acknowledging items", func() {
		amqpAddr := util.GetEnvDefault("AMQP_ADDRESS", "amqp://guest:guest@localhost:5672/")
		redisAddr := util.GetEnvDefault("REDIS_ADDRESS", "127.0.0.1:6379")
		exchangeName, queueName := "test-workflow-exchange", "test-workflow-queue"
		prefix := "govuk_mirror_crawler_workflow_test"

		var (
			queueManager    *QueueManager
			queueManagerErr error
			ttlHashSet      *TTLHashSet
			ttlHashSetErr   error
			mirrorRoot      string
		)

		BeforeEach(func() {
			mirrorRoot = os.Getenv("MIRROR_ROOT")

			ttlHashSet, ttlHashSetErr = NewTTLHashSet(prefix, redisAddr)
			Expect(ttlHashSetErr).To(BeNil())

			queueManager, queueManagerErr = NewQueueManager(
				amqpAddr,
				exchangeName,
				queueName)

			Expect(queueManagerErr).To(BeNil())
			Expect(queueManager).ToNot(BeNil())
		})

		AfterEach(func() {
			Expect(ttlHashSet.Close()).To(BeNil())
			Expect(purgeAllKeys(prefix, redisAddr)).To(BeNil())

			deleted, err := queueManager.Consumer.Channel.QueueDelete(queueName, false, false, true)
			Expect(err).To(BeNil())
			Expect(deleted).To(Equal(0))

			err = queueManager.Consumer.Channel.ExchangeDelete(exchangeName, false, true)
			Expect(err).To(BeNil())

			queueManager.Close()
			DeleteMirrorFilesFromDisk(mirrorRoot)
		})

		Describe("AcknowledgeItem", func() {
			It("should read from a channel and add URLs to the hash set", func() {
				url := "https://www.gov.uk/foo"

				exists, err := ttlHashSet.Exists(url)
				Expect(err).To(BeNil())
				Expect(exists).To(BeFalse())

				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())

				outbound := make(chan *CrawlerMessageItem, 1)

				err = queueManager.Publish("#", "text/plain", url)
				Expect(err).To(BeNil())

				for item := range deliveries {
					outbound <- NewCrawlerMessageItem(item, "www.gov.uk", []string{})
					break
				}

				Expect(len(outbound)).To(Equal(1))

				go AcknowledgeItem(outbound, ttlHashSet)
				time.Sleep(time.Millisecond)

				Expect(len(outbound)).To(Equal(0))

				exists, err = ttlHashSet.Exists(url)
				Expect(err).To(BeNil())
				Expect(exists).To(BeTrue())

				// Close the channel to stop the goroutine for AcknowledgeItem.
				close(outbound)
			})
		})

		Describe("CrawlURL", func() {
			var (
				crawler    *Crawler
				crawlerErr error
			)

			BeforeEach(func() {
				crawler, crawlerErr = NewCrawler("http://127.0.0.1/")

				Expect(crawlerErr).To(BeNil())
				Expect(crawler).ToNot(BeNil())
			})

			It("crawls a URL and assigns the body", func() {
				outbound := make(chan *CrawlerMessageItem, 1)

				body := `<a href="foo.com">bar</a>`
				server := testServer(200, body)

				deliveryItem := &amqp.Delivery{Body: []byte(server.URL)}
				outbound <- NewCrawlerMessageItem(*deliveryItem, "127.0.0.1", []string{})

				crawled := CrawlURL(outbound, crawler)

				Expect((<-crawled).HTMLBody[0:25]).To(Equal([]byte(body)))

				server.Close()
				close(outbound)
			})
		})

		Describe("WriteItemToDisk", func() {
			It("wrote the item to disk", func() {
				url := "https://www.gov.uk/extract-some-urls"
				deliveryItem := &amqp.Delivery{Body: []byte(url)}
				item := NewCrawlerMessageItem(*deliveryItem, "www.gov.uk", []string{})
				item.HTMLBody = []byte(`<a href="https://www.gov.uk/some-url">a link</a>`)

				outbound := make(chan *CrawlerMessageItem, 1)
				extract := WriteItemToDisk(outbound)

				Expect(len(extract)).To(Equal(0))

				outbound <- item

				Expect(<-extract).To(Equal(item))

				filePath, _ := item.FilePath()
				fileContent, err := ioutil.ReadFile(filePath)

				Expect(err).To(BeNil())
				Expect(fileContent).To(Equal(item.HTMLBody))

				close(outbound)
			})
		})

		Describe("ExtractURLs", func() {
			It("extracts URLs from the HTML body and adds them to a new channel; acknowledging item", func() {
				url := "https://www.gov.uk/extract-some-urls"
				deliveryItem := &amqp.Delivery{Body: []byte(url)}
				item := NewCrawlerMessageItem(*deliveryItem, "www.gov.uk", []string{})
				item.HTMLBody = []byte(`<a href="https://www.gov.uk/some-url">a link</a>`)

				outbound := make(chan *CrawlerMessageItem, 1)
				publish, acknowledge := ExtractURLs(outbound)

				Expect(len(publish)).To(Equal(0))
				Expect(len(acknowledge)).To(Equal(0))

				outbound <- item

				Expect(<-publish).To(Equal("https://www.gov.uk/some-url"))
				Expect(<-acknowledge).To(Equal(item))

				close(outbound)
			})
		})

		Describe("PublishURLs", func() {
			It("doesn't publish URLs that have already been crawled", func() {
				url := "https://www.gov.uk/government/organisations"

				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())
				Expect(len(deliveries)).To(Equal(0))

				_, err = ttlHashSet.Add(url)
				Expect(err).To(BeNil())

				publish := make(chan string, 1)
				outbound := make(chan []byte, 1)

				go func() {
					for item := range deliveries {
						outbound <- item.Body
					}
				}()
				go PublishURLs(ttlHashSet, queueManager, publish)
				time.Sleep(time.Millisecond)

				publish <- url
				time.Sleep(time.Millisecond)

				Expect(len(publish)).To(Equal(0))
				Expect(len(outbound)).To(Equal(0))

				// Close the channel to stop the goroutine for PublishURLs.
				close(publish)
				close(outbound)
			})

			It("publishes URLs that haven't been crawled yet", func() {
				url := "https://www.gov.uk/government/foo"

				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())
				Expect(len(deliveries)).To(Equal(0))

				publish := make(chan string, 1)
				outbound := make(chan []byte, 1)

				go func() {
					for item := range deliveries {
						outbound <- item.Body
					}
				}()
				go PublishURLs(ttlHashSet, queueManager, publish)

				publish <- url

				Expect(<-outbound).To(Equal([]byte(url)))
				Expect(len(publish)).To(Equal(0))

				close(publish)
				close(outbound)
			})
		})

		Describe("ReadFromQueue", func() {
			It("provides a way of converting AMQP bodies to CrawlerMessageItems", func() {
				deliveries, err := queueManager.Consume()
				Expect(err).To(BeNil())

				outbound := ReadFromQueue(deliveries, ttlHashSet, []string{})
				Expect(len(outbound)).To(Equal(0))

				url := "https://www.foo.com/bar"
				err = queueManager.Publish("#", "text/plain", url)
				Expect(err).To(BeNil())

				item := <-outbound
				Expect(string(item.Body)).To(Equal(url))

				close(outbound)
			})
		})
	})
})

func purgeAllKeys(prefix string, address string) error {
	client, err := redis.Dial("tcp", address)
	if err != nil {
		return err
	}

	keys, err := client.Cmd("KEYS", prefix+"*").List()
	if err != nil || len(keys) <= 0 {
		return err
	}

	reply := client.Cmd("DEL", keys)
	if reply.Err != nil {
		return reply.Err
	}

	return nil
}

func testServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintln(w, body)
	}))
}
