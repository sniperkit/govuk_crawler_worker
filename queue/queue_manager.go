package queue

type QueueManager struct {
	ExchangeName string
	QueueName    string
	Consumer     *QueueConnection
	Producer     *QueueConnection
}

func NewQueueManager(amqpAddr string, exchangeName string, queueName string) (*QueueManager, error) {
	consumer, err := NewQueueConnection(amqpAddr)
	if err != nil {
		return nil, err
	}

	producer, err := NewQueueConnection(amqpAddr)
	if err != nil {
		return nil, err
	}

	err = setupExchangeAndQueue(consumer, exchangeName, queueName)
	if err != nil {
		return nil, err
	}

	return &QueueManager{
		ExchangeName: exchangeName,
		QueueName:    queueName,
		Consumer:     consumer,
		Producer:     producer,
	}, nil
}

func (h *QueueManager) Close() error {
	err := h.Producer.Close()
	if err != nil {
		defer h.Consumer.Close()
		return err
	}

	return h.Consumer.Close()
}

func setupExchangeAndQueue(connection *QueueConnection, exchangeName string, queueName string) error {
	var err error

	err = connection.ExchangeDeclare(exchangeName, "topic")
	if err != nil {
		return err
	}

	_, err = connection.QueueDeclare(queueName)
	if err != nil {
		return err
	}

	return connection.BindQueueToExchange(queueName, exchangeName)
}
