// Copyright 2024 Redpanda Data, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aws

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/cenkalti/backoff/v4"

	"github.com/Jeffail/shutdown"

	"github.com/redpanda-data/benthos/v4/public/service"

	"github.com/redpanda-data/connect/v4/internal/impl/aws/config"
)

const (
	// SQS Input Fields
	sqsiFieldURL                 = "url"
	sqsiFieldWaitTimeSeconds     = "wait_time_seconds"
	sqsiFieldDeleteMessage       = "delete_message"
	sqsiFieldResetVisibility     = "reset_visibility"
	sqsiFieldMaxNumberOfMessages = "max_number_of_messages"
	sqsiFieldMaxOutstanding      = "max_outstanding_messages"
	sqsiFieldMessageTimeout      = "message_timeout"
)

type sqsiConfig struct {
	URL                 string
	WaitTimeSeconds     int
	DeleteMessage       bool
	ResetVisibility     bool
	MaxNumberOfMessages int
	MaxOutstanding      int
	MessageTimeout      time.Duration
}

func sqsiConfigFromParsed(pConf *service.ParsedConfig) (conf sqsiConfig, err error) {
	if conf.URL, err = pConf.FieldString(sqsiFieldURL); err != nil {
		return
	}
	if conf.WaitTimeSeconds, err = pConf.FieldInt(sqsiFieldWaitTimeSeconds); err != nil {
		return
	}
	if conf.DeleteMessage, err = pConf.FieldBool(sqsiFieldDeleteMessage); err != nil {
		return
	}
	if conf.ResetVisibility, err = pConf.FieldBool(sqsiFieldResetVisibility); err != nil {
		return
	}
	if conf.MaxNumberOfMessages, err = pConf.FieldInt(sqsiFieldMaxNumberOfMessages); err != nil {
		return
	}
	if conf.MaxOutstanding, err = pConf.FieldInt(sqsiFieldMaxOutstanding); err != nil {
		return
	}
	if conf.MessageTimeout, err = pConf.FieldDuration(sqsiFieldMessageTimeout); err != nil {
		return
	}
	return
}

func sqsInputSpec() *service.ConfigSpec {
	return service.NewConfigSpec().
		Stable().
		Categories("Services", "AWS").
		Summary(`Consume messages from an AWS SQS URL.`).
		Description(`
== Credentials

By default Redpanda Connect will use a shared credentials file when connecting to AWS
services. It's also possible to set them explicitly at the component level,
allowing you to transfer data across accounts. You can find out more in
xref:guides:cloud/aws.adoc[].

== Metadata

This input adds the following metadata fields to each message:

- sqs_message_id
- sqs_receipt_handle
- sqs_approximate_receive_count
- All message attributes

You can access these metadata fields using
xref:configuration:interpolation.adoc#bloblang-queries[function interpolation].`).
		Fields(
			service.NewURLField(sqsiFieldURL).
				Description("The SQS URL to consume from."),
			service.NewBoolField(sqsiFieldDeleteMessage).
				Description("Whether to delete the consumed message once it is acked. Disabling allows you to handle the deletion using a different mechanism.").
				Default(true).
				Advanced(),
			service.NewBoolField(sqsiFieldResetVisibility).
				Description("Whether to set the visibility timeout of the consumed message to zero once it is nacked. Disabling honors the preset visibility timeout specified for the queue.").
				Version("3.58.0").
				Default(true).
				Advanced(),
			service.NewIntField(sqsiFieldMaxNumberOfMessages).
				Description("The maximum number of messages to return on one poll. Valid values: 1 to 10.").
				Default(10).
				Advanced(),
			service.NewIntField(sqsiFieldMaxOutstanding).
				Description("The maximum number of outstanding pending messages to be consumed at a given time.").
				Default(1000),
			service.NewIntField(sqsiFieldWaitTimeSeconds).
				Description("Whether to set the wait time. Enabling this activates long-polling. Valid values: 0 to 20.").
				Default(0).
				Advanced(),
			service.NewDurationField(sqsiFieldMessageTimeout).
				Description("The time to process messages before needing to refresh the receipt handle. Messages will be eligible for refresh when half of the timeout has elapsed.").
				Default("30s").
				Advanced(),
		).
		Fields(config.SessionFields()...)
}

func init() {
	err := service.RegisterInput("aws_sqs", sqsInputSpec(),
		func(pConf *service.ParsedConfig, mgr *service.Resources) (service.Input, error) {
			sess, err := GetSession(context.TODO(), pConf)
			if err != nil {
				return nil, err
			}

			conf, err := sqsiConfigFromParsed(pConf)
			if err != nil {
				return nil, err
			}

			return newAWSSQSReader(conf, sess, mgr.Logger())
		})
	if err != nil {
		panic(err)
	}
}

//------------------------------------------------------------------------------

type sqsAPI interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessageBatch(context.Context, *sqs.DeleteMessageBatchInput, ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
	ChangeMessageVisibilityBatch(context.Context, *sqs.ChangeMessageVisibilityBatchInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityBatchOutput, error)
	SendMessageBatch(context.Context, *sqs.SendMessageBatchInput, ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
}

type awsSQSReader struct {
	conf sqsiConfig

	aconf aws.Config
	sqs   sqsAPI

	messagesChan     chan sqsMessage
	ackMessagesChan  chan *sqsMessageHandle
	nackMessagesChan chan *sqsMessageHandle
	closeSignal      *shutdown.Signaller

	log *service.Logger
}

func newAWSSQSReader(conf sqsiConfig, aconf aws.Config, log *service.Logger) (*awsSQSReader, error) {
	return &awsSQSReader{
		conf:             conf,
		aconf:            aconf,
		log:              log,
		messagesChan:     make(chan sqsMessage),
		ackMessagesChan:  make(chan *sqsMessageHandle),
		nackMessagesChan: make(chan *sqsMessageHandle),
		closeSignal:      shutdown.NewSignaller(),
	}, nil
}

// Connect attempts to establish a connection to the target SQS
// queue.
func (a *awsSQSReader) Connect(ctx context.Context) error {
	if a.sqs == nil {
		a.sqs = sqs.NewFromConfig(a.aconf)
	}

	ift := &sqsInFlightTracker{
		handles: map[string]sqsInFlightHandle{},
		limit:   a.conf.MaxOutstanding,
		timeout: a.conf.MessageTimeout,
	}
	ift.l = sync.NewCond(&ift.m)

	var wg sync.WaitGroup
	wg.Add(2)
	go a.readLoop(&wg, ift)
	go a.ackLoop(&wg, ift)
	go func() {
		wg.Wait()
		a.closeSignal.TriggerHasStopped()
	}()
	return nil
}

type sqsInFlightTracker struct {
	handles map[string]*sqsMessageHandle
	limit   int
	timeout time.Duration
	m       sync.Mutex
	l       *sync.Cond
}

func (t *sqsInFlightTracker) PullToRefresh() []*sqsMessageHandle {
	t.m.Lock()
	defer t.m.Unlock()

	handles := make([]*sqsMessageHandle, 0, len(t.handles))
	now := time.Now()
	for _, v := range t.handles {
		// Only update messages when we get to half the timeout
		//
		// This prevents N^2 refresh behavior.
		if v.deadline.Load().Sub(now) < (t.timeout / 2) {
			continue
		}
		handles = append(handles, v)
		v.deadline.Store(now.Add(t.timeout))
	}
	return handles
}

func (t *sqsInFlightTracker) Size() int {
	t.m.Lock()
	defer t.m.Unlock()
	return len(t.handles)
}

func (t *sqsInFlightTracker) Remove(id string) {
	t.m.Lock()
	defer t.m.Unlock()
	delete(t.handles, id)
	t.l.Signal()
}

func (t *sqsInFlightTracker) Clear() {
	t.m.Lock()
	defer t.m.Unlock()
	clear(t.handles)
	t.l.Signal()
}

func (t *sqsInFlightTracker) AddNew(ctx context.Context, messages ...sqsMessage) {
	t.m.Lock()
	defer t.m.Unlock()

	// Treat this as a soft limit, we can burst over, but we should be able to make progress.
	for len(t.handles) >= t.limit {
		if ctx.Err() != nil {
			return
		}
		t.l.Wait()
	}

	for _, m := range messages {
		if m.handle == nil {
			continue
		}
		t.handles[m.handle.id] = m.handle
	}
}

func (a *awsSQSReader) ackLoop(wg *sync.WaitGroup, inFlightTracker *sqsInFlightTracker) {
	defer wg.Done()
	defer inFlightTracker.Clear()

	closeNowCtx, done := a.closeSignal.HardStopCtx(context.Background())
	defer done()

	flushFinishedHandles := func(handles []*sqsMessageHandle, erase bool) {
		if len(handles) == 0 {
			return
		}
		if erase {
			if err := a.deleteMessages(closeNowCtx, handles...); err != nil {
				a.log.Errorf("Failed to delete messages: %v", err)
			}
		} else {
			if err := a.resetMessages(closeNowCtx, handles...); err != nil {
				a.log.Errorf("Failed to reset the visibility timeout of messages: %v", err)
			}
		}
	}

	var refreshLock sync.Mutex // This should probably be another seperate loop...
	refreshCurrentHandles := func() int {
		if !refreshLock.TryLock() {
			return 0
		}
		defer refreshLock.Unlock()
		currentHandles := inFlightTracker.PullToRefresh()
		if len(currentHandles) == 0 {
			return 0
		}
		d := time.Now()
		if err := a.updateVisibilityMessages(closeNowCtx, int(a.conf.MessageTimeout.Seconds()), currentHandles...); err != nil {
			a.log.Debugf("Failed to update messages visibility timeout: %v", err)
		}
		a.log.Debugf("refreshed %d handles in %v", len(currentHandles), time.Since(d))
		return len(currentHandles)
	}

	flushTimer := time.NewTicker(time.Second)
	defer flushTimer.Stop()

	pendingAcks := []*sqsMessageHandle{}
	pendingNacks := []*sqsMessageHandle{}

ackLoop:
	for {
		select {
		case h := <-a.ackMessagesChan:
			a.log.Debugf("[ackloop] acking msg (pa=%v, pn=%v, t=%v)", len(pendingAcks), len(pendingNacks), inFlightTracker.Size())
			t := time.Now()
			pendingAcks = append(pendingAcks, h)
			inFlightTracker.Remove(h.id)
			h.deadline.SetDeleted()
			if len(pendingAcks) >= a.conf.MaxNumberOfMessages {
				flushFinishedHandles(pendingAcks, true)
			}
			a.log.Debugf("[ackloop] done handling ack (d=%v, pa=%v t=%v)", time.Since(t), len(pendingAcks), inFlightTracker.Size())
		case h := <-a.nackMessagesChan:
			a.log.Debugf("[ackloop] nacking msg (pa=%v, pn=%v, t=%v)", len(pendingAcks), len(pendingNacks), inFlightTracker.Size())
			t := time.Now()
			pendingNacks = append(pendingNacks, h)
			inFlightTracker.Remove(h.id)
			h.deadline.SetDeleted()
			if len(pendingNacks) >= a.conf.MaxNumberOfMessages {
				flushFinishedHandles(pendingNacks, false)
			}
			a.log.Debugf("[ackloop] done handling nack (d=%v, pn=%v t=%v)", time.Since(t), len(pendingNacks), inFlightTracker.Size())
		case <-flushTimer.C:
			a.log.Debugf("[ackloop] flushing all (pa=%v, pn=%v, t=%v)", len(pendingAcks), len(pendingNacks), inFlightTracker.Size())
			t := time.Now()
			flushFinishedHandles(pendingAcks, true)
			flushFinishedHandles(pendingNacks, false)
			go refreshCurrentHandles()
			a.log.Debugf("[ackloop] flushed all (d=%v, t=%v)", time.Since(t), inFlightTracker.Size())
		case <-a.closeSignal.SoftStopChan():
			break ackLoop
		}
	}

	flushFinishedHandles(pendingAcks, true)
	flushFinishedHandles(pendingNacks, false)
}

func (a *awsSQSReader) readLoop(wg *sync.WaitGroup, inFlightTracker *sqsInFlightTracker) {
	defer wg.Done()

	var pendingMsgs []sqsMessage
	defer func() {
		if len(pendingMsgs) > 0 {
			tmpNacks := make([]*sqsMessageHandle, 0, len(pendingMsgs))
			for _, m := range pendingMsgs {
				if m.handle == nil {
					continue
				}
				tmpNacks = append(tmpNacks, m.handle)
			}
			ctx, done := a.closeSignal.HardStopCtx(context.Background())
			defer done()
			if err := a.resetMessages(ctx, tmpNacks...); err != nil {
				a.log.Errorf("Failed to reset visibility timeout for pending messages: %v", err)
			}
		}
	}()

	closeAtLeisureCtx, done := a.closeSignal.SoftStopCtx(context.Background())
	defer done()

	backoff := backoff.NewExponentialBackOff()
	backoff.InitialInterval = 10 * time.Millisecond
	backoff.MaxInterval = time.Minute
	backoff.MaxElapsedTime = 0

	getMsgs := func() {
		res, err := a.sqs.ReceiveMessage(closeAtLeisureCtx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(a.conf.URL),
			MaxNumberOfMessages:   int32(a.conf.MaxNumberOfMessages),
			WaitTimeSeconds:       int32(a.conf.WaitTimeSeconds),
			AttributeNames:        []types.QueueAttributeName{types.QueueAttributeNameAll},
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			if !awsErrIsTimeout(err) {
				a.log.Errorf("Failed to pull new SQS messages: %v", err)
			}
			return
		}
		if len(res.Messages) > 0 {
			a.log.Tracef("adding new msgs (n=%v, t=%v)", len(res.Messages), inFlightTracker.Size())
			for _, msg := range res.Messages {
				var handle *sqsMessageHandle
				if msg.MessageId != nil && msg.ReceiptHandle != nil {
					handle = &sqsMessageHandle{
						id:            *msg.MessageId,
						receiptHandle: *msg.ReceiptHandle,
					}
					handle.deadline.Store(time.Now().Add(a.conf.MessageTimeout))
				}
				pendingMsgs = append(pendingMsgs, sqsMessage{
					Message: msg,
					handle:  handle,
				})
			}
			inFlightTracker.AddNew(closeAtLeisureCtx, pendingMsgs[len(pendingMsgs)-len(res.Messages):]...)
		}
		if len(res.Messages) > 0 || a.conf.WaitTimeSeconds > 0 {
			// When long polling we want to reset our back off even if we didn't
			// receive messages. However, with long polling disabled we back off
			// each time we get an empty response.
			backoff.Reset()
		}
	}

	for {
		if len(pendingMsgs) == 0 {
			getMsgs()
			if len(pendingMsgs) == 0 {
				select {
				case <-time.After(backoff.NextBackOff()):
				case <-a.closeSignal.SoftStopChan():
					return
				}
				continue
			}
		}
		select {
		case a.messagesChan <- pendingMsgs[0]:
			pendingMsgs = pendingMsgs[1:]
		case <-a.closeSignal.SoftStopChan():
			return
		}
	}
}

type sqsMessage struct {
	types.Message
	handle *sqsMessageHandle
}

// Unix seconds of when the message timeout expires.
//
// If -1 then the message has been deleted.
type sqsMessageDeadline atomic.Int64

func (s *sqsMessageDeadline) Load() time.Time {
	return time.Unix((*atomic.Int64)(s).Load(), 0)
}
func (s *sqsMessageDeadline) Store(v time.Time) {
	(*atomic.Int64)(s).Store(v.Unix())
}
func (s *sqsMessageDeadline) SetDeleted() {
	(*atomic.Int64)(s).Store(-1)
}
func (s *sqsMessageDeadline) IsDeleted() bool {
	return (*atomic.Int64)(s).Load() == -1
}

type sqsMessageHandle struct {
	id, receiptHandle string
	deadline          sqsMessageDeadline
}

func (a *awsSQSReader) deleteMessages(ctx context.Context, msgs ...*sqsMessageHandle) error {
	if !a.conf.DeleteMessage {
		return nil
	}
	const maxBatchSize = 10
	for len(msgs) > 0 {
		input := sqs.DeleteMessageBatchInput{
			QueueUrl: aws.String(a.conf.URL),
			Entries:  []types.DeleteMessageBatchRequestEntry{},
		}

		for i := range msgs {
			msg := msgs[i]
			input.Entries = append(input.Entries, types.DeleteMessageBatchRequestEntry{
				Id:            &msg.id,
				ReceiptHandle: &msg.receiptHandle,
			})
			if len(input.Entries) == maxBatchSize {
				break
			}
		}

		msgs = msgs[len(input.Entries):]
		response, err := a.sqs.DeleteMessageBatch(ctx, &input)
		if err != nil {
			return err
		}
		for _, fail := range response.Failed {
			msg := "(no message)"
			if fail.Message != nil {
				msg = *fail.Message
			}
			a.log.Errorf("Failed to delete consumed SQS message '%v', response code: %v, message: %q, sender fault: %v", *fail.Id, *fail.Code, msg, fail.SenderFault)
		}
	}
	return nil
}

func (a *awsSQSReader) resetMessages(ctx context.Context, msgs ...*sqsMessageHandle) error {
	if !a.conf.ResetVisibility {
		return nil
	}
	return a.updateVisibilityMessages(ctx, 0, msgs...)
}

func (a *awsSQSReader) updateVisibilityMessages(ctx context.Context, timeout int, msgs ...*sqsMessageHandle) error {
	const maxBatchSize = 10
	for len(msgs) > 0 {
		input := sqs.ChangeMessageVisibilityBatchInput{
			QueueUrl: aws.String(a.conf.URL),
			Entries:  []types.ChangeMessageVisibilityBatchRequestEntry{},
		}

		var sent []*sqsMessageHandle
		var consumed int
		for i := range msgs {
			msg := msgs[i]
			consumed++
			if msg.deadline.IsDeleted() {
				continue
			}
			input.Entries = append(input.Entries, types.ChangeMessageVisibilityBatchRequestEntry{
				Id:                &msg.id,
				ReceiptHandle:     &msg.receiptHandle,
				VisibilityTimeout: int32(timeout),
			})
			sent = append(sent, msg)
			if len(input.Entries) == maxBatchSize {
				break
			}
		}

		msgs = msgs[consumed:]
		response, err := a.sqs.ChangeMessageVisibilityBatch(ctx, &input)
		if err != nil {
			return err
		}
		for i, fail := range response.Failed {
			if sent[i].deadline.IsDeleted() {
				continue
			}
			msg := "(no message)"
			if fail.Message != nil {
				msg = *fail.Message
			}
			a.log.Debugf("Failed to update consumed SQS message '%v' visibility, response code: %v, message: %q, sender fault: %v", *fail.Id, *fail.Code, msg, fail.SenderFault)
		}
	}
	return nil
}

func addSQSMetadata(p *service.Message, sqsMsg types.Message) {
	p.MetaSetMut("sqs_message_id", *sqsMsg.MessageId)
	p.MetaSetMut("sqs_receipt_handle", *sqsMsg.ReceiptHandle)
	if rCountStr, exists := sqsMsg.Attributes["ApproximateReceiveCount"]; exists {
		p.MetaSetMut("sqs_approximate_receive_count", rCountStr)
	}
	for k, v := range sqsMsg.MessageAttributes {
		if v.StringValue != nil {
			p.MetaSetMut(k, *v.StringValue)
		}
	}
}

// ReadBatch attempts to read a new message from the target SQS.
func (a *awsSQSReader) Read(ctx context.Context) (*service.Message, service.AckFunc, error) {
	if a.sqs == nil {
		return nil, nil, service.ErrNotConnected
	}

	var next sqsMessage
	var open bool
	select {
	case next, open = <-a.messagesChan:
		if !open {
			return nil, nil, service.ErrEndOfInput
		}
	case <-a.closeSignal.SoftStopChan():
		return nil, nil, service.ErrEndOfInput
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	if next.Body == nil {
		return nil, nil, context.Canceled
	}

	msg := service.NewMessage([]byte(*next.Body))
	addSQSMetadata(msg, next.Message)
	mHandle := next.handle
	return msg, func(rctx context.Context, res error) error {
		if mHandle == nil {
			return nil
		}
		if res == nil {
			select {
			case <-rctx.Done():
				return rctx.Err()
			case <-a.closeSignal.SoftStopChan():
				return a.deleteMessages(rctx, mHandle)
			case a.ackMessagesChan <- mHandle:
			}
			return nil
		}

		select {
		case <-rctx.Done():
			return rctx.Err()
		case <-a.closeSignal.SoftStopChan():
			return a.resetMessages(rctx, mHandle)
		case a.nackMessagesChan <- mHandle:
		}
		return nil
	}, nil
}

func (a *awsSQSReader) Close(ctx context.Context) error {
	a.closeSignal.TriggerSoftStop()

	var closeNowAt time.Duration
	if dline, ok := ctx.Deadline(); ok {
		if closeNowAt = time.Until(dline) - time.Second; closeNowAt <= 0 {
			a.closeSignal.TriggerHardStop()
		}
	}
	if closeNowAt > 0 {
		select {
		case <-time.After(closeNowAt):
			a.closeSignal.TriggerHardStop()
		case <-ctx.Done():
			return ctx.Err()
		case <-a.closeSignal.HasStoppedChan():
			return nil
		}
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.closeSignal.HasStoppedChan():
	}
	return nil
}
