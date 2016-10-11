package main

import (
	"flag"
	"net"
	"net/http"

	"encoding/json"
	_ "expvar"
	"fmt"
	"os"

	"github.com/cyverse-de/templeton/database"
	"github.com/cyverse-de/templeton/elasticsearch"
	"github.com/cyverse-de/templeton/model"
	"github.com/johnworth/events/ping"

	"github.com/cyverse-de/configurate"
	"github.com/cyverse-de/logcabin"
	"github.com/cyverse-de/messaging"
	"github.com/spf13/viper"
	"github.com/streadway/amqp"
)

const defaultConfig = `
amqp:
  uri: amqp://guest:guest@rabbit:5672/

elasticsearch:
  base: http://elasticsearch:9200
  index: data

db:
  uri: postgres://de:notprod@dedb:5432/metadata?sslmode=disable
`

var (
	showVersion        = flag.Bool("version", false, "Print version information")
	mode               = flag.String("mode", "", "One of 'periodic', 'incremental', or 'full'. Required except for --version.")
	debugPort          = flag.String("debug-port", "60000", "Listen port for requests to /debug/vars.")
	cfgPath            = flag.String("config", "", "Path to the configuration file. Required except for --version.")
	amqpURI            string
	amqpExchangeName   string
	amqpExchangeType   string
	elasticsearchBase  string
	elasticsearchIndex string
	dbURI              string
	cfg                *viper.Viper
)

func init() {
	flag.Parse()
	logcabin.Init("templeton", "templeton")
}

func checkMode() {
	validModes := []string{"periodic", "incremental", "full"}
	foundMode := false

	for _, v := range validModes {
		if v == *mode {
			foundMode = true
		}
	}

	if !foundMode {
		fmt.Printf("Invalid mode: %s\n", *mode)
		flag.PrintDefaults()
		os.Exit(-1)
	}
}

func initConfig(cfgPath string) {
	var err error
	cfg, err = configurate.InitDefaults(cfgPath, defaultConfig)
	if err != nil {
		logcabin.Error.Fatal(err)
	}
}

func loadElasticsearchConfig() {
	elasticsearchBase = cfg.GetString("elasticsearch.base")
	elasticsearchIndex = cfg.GetString("elasticsearch.index")
}

func loadAMQPConfig() {
	amqpURI = cfg.GetString("amqp.uri")
	amqpExchangeName = cfg.GetString("amqp.exchange.name")
	amqpExchangeType = cfg.GetString("amqp.exchange.type")
}

func loadDBConfig() {
	dbURI = cfg.GetString("db.uri")
}

func doFullMode(es *elasticsearch.Elasticer, d *database.Databaser) {
	logcabin.Info.Println("Full indexing mode selected.")

	es.Reindex(d)
}

// A spinner to keep the program running, since client.Listen() needs to be in a goroutine.
func spin() {
	spinner := make(chan int)
	for {
		select {
		case <-spinner:
			fmt.Println("Exiting")
			break
		}
	}
}

func doPeriodicMode(es *elasticsearch.Elasticer, d *database.Databaser, client *messaging.Client) {
	logcabin.Info.Println("Periodic indexing mode selected.")

	// Accept and handle messages sent out with the index.all and index.templates routing keys
	client.AddConsumerMulti(
		amqpExchangeName,
		amqpExchangeType,
		"templeton.periodic",
		[]string{messaging.ReindexAllKey, messaging.ReindexTemplatesKey},
		func(del amqp.Delivery) {
			logcabin.Info.Printf("Recieved message: [%s] [%s]", del.RoutingKey, del.Body)

			es.Reindex(d)
			del.Ack(false)
		})

	spin()
}

func doIncrementalMode(es *elasticsearch.Elasticer, d *database.Databaser, client *messaging.Client) {
	logcabin.Info.Println("Incremental indexing mode selected.")

	client.AddConsumer(
		amqpExchangeName,
		amqpExchangeType,
		"templeton.incremental",
		messaging.IncrementalKey,
		func(del amqp.Delivery) {
			logcabin.Info.Printf("Recieved message: [%s] [%s]", del.RoutingKey, del.Body)

			var m model.UpdateMessage
			err := json.Unmarshal(del.Body, &m)
			if err != nil {
				logcabin.Error.Print(err)
				del.Reject(!del.Redelivered)
			}
			es.IndexOne(d, m.ID)
			del.Ack(false)
		})

	spin()
}

func handlePing(client *messaging.Client, delivery amqp.Delivery, mode string) {
	logcabin.Info.Println("Received ping")

	pongKey := fmt.Sprintf("events.templeton.%s.pong", mode)

	out, err := json.Marshal(&ping.Pong{
		PongFrom: fmt.Sprintf("templeton-%s", mode),
	})
	if err != nil {
		logcabin.Error.Print(err)
	}

	logcabin.Info.Println("Sent pong")

	if err = client.Publish(pongKey, out); err != nil {
		logcabin.Error.Print(err)
	}
}

func listenForEvents(client *messaging.Client, mode string) {
	logcabin.Info.Println("Setting up support for events")

	eventsKey := fmt.Sprintf("events.templeton.%s.#", mode)
	pingKey := fmt.Sprintf("events.templeton.%s.ping", mode)

	client.SetupPublishing(amqpExchangeName)

	client.AddConsumer(
		amqpExchangeName,
		amqpExchangeType,
		fmt.Sprintf("events.templeton.%s.queue", mode),
		eventsKey,
		func(delivery amqp.Delivery) {
			delivery.Ack(false)
			logcabin.Info.Printf("Received event message: [%s] [%s]", delivery.RoutingKey, delivery.Body)
			switch delivery.RoutingKey {
			case pingKey:
				handlePing(client, delivery, mode)
			default:
				logcabin.Info.Printf("No handler for message: [%s] [%s]", delivery.RoutingKey, delivery.Body)
			}
		},
	)
}

func exportVars(port string) {
	go func() {
		sock, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%s", port))
		if err != nil {
			logcabin.Error.Fatal(err)
		}
		http.Serve(sock, nil)
	}()
}

var (
	gitref  string
	appver  string
	builtby string
)

// AppVersion prints the version information to stdout
func AppVersion() {
	if appver != "" {
		fmt.Printf("App-Version: %s\n", appver)
	}
	if gitref != "" {
		fmt.Printf("Git-Ref: %s\n", gitref)
	}
	if builtby != "" {
		fmt.Printf("Built-By: %s\n", builtby)
	}
}

func main() {
	if *showVersion {
		AppVersion()
		os.Exit(0)
	}

	checkMode()

	if *cfgPath == "" {
		fmt.Println("--config is required")
		flag.PrintDefaults()
		os.Exit(-1)
	}

	initConfig(*cfgPath)
	loadElasticsearchConfig()
	es, err := elasticsearch.NewElasticer(elasticsearchBase, elasticsearchIndex)
	if err != nil {
		logcabin.Error.Fatal(err)
	}
	defer es.Close()

	loadDBConfig()
	d, err := database.NewDatabaser(dbURI)
	if err != nil {
		logcabin.Error.Fatal(err)
	}

	if *mode == "full" {
		doFullMode(es, d)
		return
	}

	loadAMQPConfig()

	client, err := messaging.NewClient(amqpURI, true)
	if err != nil {
		logcabin.Error.Fatal(err)
	}
	defer client.Close()

	exportVars(*debugPort)

	go client.Listen()

	listenForEvents(client, *mode)

	if *mode == "periodic" {
		doPeriodicMode(es, d, client)
	}

	if *mode == "incremental" {
		doIncrementalMode(es, d, client)
	}
}
