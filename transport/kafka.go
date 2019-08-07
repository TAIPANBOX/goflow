package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	flowmessage "github.com/cloudflare/goflow/pb"
	"github.com/cloudflare/goflow/utils"
	proto "github.com/golang/protobuf/proto"
	//"github.com/golang/protobuf/descriptor"
	"errors"
	"flag"
	sarama "github.com/Shopify/sarama"
	"os"
	"reflect"
	"strings"
)

var (
	KafkaTLS   *bool
	KafkaSASL  *bool
	KafkaTopic *string
	KafkaSrv   *string
	KafkaBrk   *string

	KafkaLogErrors *bool

	KafkaHashing *bool
	KafkaKeying  *string
)

type KafkaState struct {
	producer sarama.AsyncProducer
	topic    string
	hashing  bool
	keying   []string
}

func RegisterFlags() {
	KafkaTLS = flag.Bool("kafka.tls", false, "Use TLS to connect to Kafka")
	KafkaSASL = flag.Bool("kafka.sasl", false, "Use SASL/PLAIN data to connect to Kafka (TLS is recommended and the environment variables KAFKA_SASL_USER and KAFKA_SASL_PASS need to be set)")
	KafkaTopic = flag.String("kafka.topic", "flow-messages", "Kafka topic to produce to")
	KafkaSrv = flag.String("kafka.srv", "", "SRV record containing a list of Kafka brokers (or use kafka.out.brokers)")
	KafkaBrk = flag.String("kafka.brokers", "127.0.0.1:9092,[::1]:9092", "Kafka brokers list separated by commas")

	KafkaLogErrors = flag.Bool("kafka.log.err", false, "Log Kafka errors")

	KafkaHashing = flag.Bool("kafka.hashing", false, "Enable partitioning by hash instead of random")
	KafkaKeying = flag.String("kafka.key", "SamplerAddr,DstAS", "Kafka list of fields to do hashing on (partition) separated by commas")
}

func StartKafkaProducerFromArgs(log utils.Logger) (*KafkaState, error) {
	addrs := make([]string, 0)
	if *KafkaSrv != "" {
		addrs, _ = utils.GetServiceAddresses(*KafkaSrv)
	} else {
		addrs = strings.Split(*KafkaBrk, ",")
	}
	return StartKafkaProducer(addrs, *KafkaTopic, *KafkaHashing, *KafkaKeying, *KafkaTLS, *KafkaSASL, *KafkaLogErrors, log)
}

func StartKafkaProducer(addrs []string, topic string, hashing bool, keying string, useTls bool, useSasl bool, logErrors bool, log utils.Logger) (*KafkaState, error) {
	kafkaConfig := sarama.NewConfig()
	kafkaConfig.Producer.Return.Successes = false
	kafkaConfig.Producer.Return.Errors = logErrors
	if useTls {
		rootCAs, err := x509.SystemCertPool()
		if err != nil {
			return nil, errors.New(fmt.Sprintf("Error initializing TLS: %v", err))
		}
		kafkaConfig.Net.TLS.Enable = true
		kafkaConfig.Net.TLS.Config = &tls.Config{RootCAs: rootCAs}
	}

	var keyingSplit []string
	if hashing {
		kafkaConfig.Producer.Partitioner = sarama.NewHashPartitioner
		keyingSplit = strings.Split(keying, ",")
	}

	if useSasl {
		if !useTls && log != nil {
			log.Warn("Using SASL without TLS will transmit the authentication in plaintext!")
		}
		kafkaConfig.Net.SASL.Enable = true
		kafkaConfig.Net.SASL.User = os.Getenv("KAFKA_SASL_USER")
		kafkaConfig.Net.SASL.Password = os.Getenv("KAFKA_SASL_PASS")
		if kafkaConfig.Net.SASL.User == "" && kafkaConfig.Net.SASL.Password == "" {
			return nil, errors.New("Kafka SASL config from environment was unsuccessful. KAFKA_SASL_USER and KAFKA_SASL_PASS need to be set.")
		} else if log != nil {
			log.Infof("Authenticating as user '%s'...", kafkaConfig.Net.SASL.User)
		}
	}

	kafkaProducer, err := sarama.NewAsyncProducer(addrs, kafkaConfig)
	if err != nil {
		return nil, err
	}
	state := KafkaState{
		producer: kafkaProducer,
		topic:    topic,
		hashing:  hashing,
		keying:   keyingSplit,
	}

	if logErrors {
		go func() {
			for {
				select {
				case msg := <-kafkaProducer.Errors():
					if log != nil {
						log.Error(msg)
					}
				}
			}
		}()
	}

	return &state, nil
}

func HashProto(fields []string, flowMessage *flowmessage.FlowMessage) string {
	var keyStr string

	if flowMessage != nil {
		vfm := reflect.ValueOf(flowMessage)
		vfm = reflect.Indirect(vfm)

		for _, kf := range fields {
			fieldValue := vfm.FieldByName(kf)
			if fieldValue.IsValid() {
				keyStr += fmt.Sprintf("%v-", fieldValue)
			}
		}
	}

	return keyStr
}

func (s KafkaState) SendKafkaFlowMessage(flowMessage *flowmessage.FlowMessage) {
	var key sarama.Encoder
	if s.hashing {
		keyStr := HashProto(s.keying, flowMessage)
		key = sarama.StringEncoder(keyStr)
	}
	b, _ := proto.Marshal(flowMessage)
	s.producer.Input() <- &sarama.ProducerMessage{
		Topic: s.topic,
		Key:   key,
		Value: sarama.ByteEncoder(b),
	}
}

func (s KafkaState) Publish(msgs []*flowmessage.FlowMessage) {
	for _, msg := range msgs {
		s.SendKafkaFlowMessage(msg)
	}
}
