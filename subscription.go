/*
Copyright 2014 The go-marathon Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package marathon

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/udacity/eventsource"
)

// Subscriptions is a collection to urls that marathon is implementing a callback on
type Subscriptions struct {
	CallbackURLs []string `json:"callbackUrls"`
}

// Subscriptions retrieves a list of registered subscriptions
func (r *marathonClient) Subscriptions() (*Subscriptions, error) {
	subscriptions := new(Subscriptions)
	if err := r.apiGet(marathonAPISubscription, nil, subscriptions); err != nil {
		return nil, err
	}

	return subscriptions, nil
}

// AddEventsListener adds your self as a listener to events from Marathon
//		channel:	a EventsChannel used to receive event on
func (r *marathonClient) AddEventsListener(filter int) (EventsChannel, error) {
	r.Lock()
	defer r.Unlock()

	// step: someone has asked to start listening to event, we need to register for events
	// if we haven't done so already
	if err := r.registerSubscription(); err != nil {
		return nil, err
	}

	channel := make(EventsChannel)
	r.listeners[channel] = EventsChannelContext{
		filter:     filter,
		done:       make(chan struct{}, 1),
		completion: &sync.WaitGroup{},
	}
	return channel, nil
}

// RemoveEventsListener removes the channel from the events listeners
//		channel:			the channel you are removing
func (r *marathonClient) RemoveEventsListener(channel EventsChannel) {
	r.Lock()
	defer r.Unlock()

	if context, found := r.listeners[channel]; found {
		close(context.done)
		delete(r.listeners, channel)
		// step: if there is no one else listening, let's remove ourselves
		// from the events callback
		if r.config.EventsTransport == EventsTransportCallback && len(r.listeners) == 0 {
			r.Unsubscribe(r.SubscriptionURL())
		}

		// step: wait for pending goroutines to finish and close channel
		go func(completion *sync.WaitGroup) {
			completion.Wait()
			close(channel)
		}(context.completion)
	}
}

// SubscriptionURL retrieves the subscription callback URL used when registering
func (r *marathonClient) SubscriptionURL() string {
	if r.config.CallbackURL != "" {
		return fmt.Sprintf("%s%s", r.config.CallbackURL, defaultEventsURL)
	}

	return fmt.Sprintf("http://%s:%d%s", r.ipAddress, r.config.EventsPort, defaultEventsURL)
}

// registerSubscription registers ourselves with Marathon to receive events from configured transport facility
func (r *marathonClient) registerSubscription() error {
	switch r.config.EventsTransport {
	case EventsTransportCallback:
		return r.registerCallbackSubscription()
	case EventsTransportSSE:
		return r.registerSSESubscription()
	default:
		return fmt.Errorf("the events transport: %d is not supported", r.config.EventsTransport)
	}
}

func (r *marathonClient) registerCallbackSubscription() error {
	if r.eventsHTTP == nil {
		ipAddress, err := getInterfaceAddress(r.config.EventsInterface)
		if err != nil {
			return fmt.Errorf("Unable to get the ip address from the interface: %s, error: %s",
				r.config.EventsInterface, err)
		}

		// step: set the ip address
		r.ipAddress = ipAddress
		binding := fmt.Sprintf("%s:%d", ipAddress, r.config.EventsPort)
		// step: register the handler
		http.HandleFunc(defaultEventsURL, r.handleCallbackEvent)
		// step: create the http server
		r.eventsHTTP = &http.Server{
			Addr:           binding,
			Handler:        nil,
			ReadTimeout:    10 * time.Second,
			WriteTimeout:   10 * time.Second,
			MaxHeaderBytes: 1 << 20,
		}

		// @todo need to add a timeout value here
		listener, err := net.Listen("tcp", binding)
		if err != nil {
			return nil
		}

		go func() {
			for {
				r.eventsHTTP.Serve(listener)
			}
		}()
	}

	// step: get the callback url
	callback := r.SubscriptionURL()

	// step: check if the callback is registered
	found, err := r.HasSubscription(callback)
	if err != nil {
		return err
	}
	if !found {
		// step: we need to register ourselves
		if err := r.Subscribe(callback); err != nil {
			return err
		}
	}

	return nil
}

// registerSSESubscription starts a go routine that continously tries to
// connect to the SSE stream and to process the received events. To establish
// the connection it tries the active cluster members until no more member is
// active. When this happens it will retry to get a connection every 5 seconds.
func (r *marathonClient) registerSSESubscription() error {
	if r.subscribedToSSE {
		return nil
	}

	go func() {
		for {
			stream, err := r.connectToSSE()
			if err != nil {
				r.debugLog("Error connecting SSE subscription: %s", err)
				<-time.After(5 * time.Second)
				continue
			}
			err = r.listenToSSE(stream)
			stream.Close()
			r.debugLog("Error on SSE subscription: %s", err)
		}
	}()

	r.subscribedToSSE = true
	return nil
}

// connectToSSE tries to establish an *eventsource.Stream to any of the Marathon cluster members, marking the
// member as down on connection failure, until there is no more active member in the cluster.
// Given the http request can not be built, it will panic as this case should never happen.
func (r *marathonClient) connectToSSE() (*eventsource.Stream, error) {
	for {
		request, member, err := r.buildAPIRequest("GET", marathonAPIEventStream, nil)
		if err != nil {
			switch err.(type) {
			case newRequestError:
				panic(fmt.Sprintf("Requests for SSE subscriptions should never fail to be created: %s", err.Error()))
			default:
				return nil, err
			}
		}

		stream, err := eventsource.SubscribeWith("", r.config.HTTPSSEClient, request)
		if err != nil {
			r.debugLog("Error subscribing to Marathon event stream: %s", err)
			r.hosts.markDown(member)
			continue
		}

		return stream, nil
	}
}

func (r *marathonClient) listenToSSE(stream *eventsource.Stream) error {
	for {
		select {
		case ev := <-stream.Events:
			if err := r.handleEvent(ev.Data()); err != nil {
				r.debugLog("listenToSSE(): failed to handle event: %v", err)
			}
		case err := <-stream.Errors:
			return err

		}
	}
}

// Subscribe adds a URL to Marathon's callback facility
//	callback	: the URL you wish to subscribe
func (r *marathonClient) Subscribe(callback string) error {
	path := fmt.Sprintf("%s?callbackUrl=%s", marathonAPISubscription, callback)
	return r.apiPost(path, "", nil)

}

// Unsubscribe removes a URL from Marathon's callback facility
//	callback	: the URL you wish to unsubscribe
func (r *marathonClient) Unsubscribe(callback string) error {
	// step: remove from the list of subscriptions
	return r.apiDelete(fmt.Sprintf("%s?callbackUrl=%s", marathonAPISubscription, callback), nil, nil)
}

// HasSubscription checks to see a subscription already exists with Marathon
//		callback:			the url of the callback
func (r *marathonClient) HasSubscription(callback string) (bool, error) {
	// step: generate our events callback
	subscriptions, err := r.Subscriptions()
	if err != nil {
		return false, err
	}

	for _, subscription := range subscriptions.CallbackURLs {
		if callback == subscription {
			return true, nil
		}
	}

	return false, nil
}

func (r *marathonClient) handleEvent(content string) error {
	// step: process and decode the event
	eventType := new(EventType)
	err := json.NewDecoder(strings.NewReader(content)).Decode(eventType)
	if err != nil {
		return fmt.Errorf("failed to decode the event type, content: %s, error: %s", content, err)
	}

	// step: check whether event type is handled
	event, err := GetEvent(eventType.EventType)
	if err != nil {
		return fmt.Errorf("unable to handle event, type: %s, error: %s", eventType.EventType, err)
	}

	// step: let's decode message
	err = json.NewDecoder(strings.NewReader(content)).Decode(event.Event)
	if err != nil {
		return fmt.Errorf("failed to decode the event, id: %d, error: %s", event.ID, err)
	}

	r.RLock()
	defer r.RUnlock()

	// step: check if anyone is listen for this event
	for channel, context := range r.listeners {
		// step: check if this listener wants this event type
		if event.ID&context.filter != 0 {
			context.completion.Add(1)
			go func(ch EventsChannel, context EventsChannelContext, e *Event) {
				defer context.completion.Done()
				select {
				case ch <- e:
				case <-context.done:
					// Terminates goroutine.
				}
			}(channel, context, event)
		}
	}

	return nil
}

func (r *marathonClient) handleCallbackEvent(writer http.ResponseWriter, request *http.Request) {
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		// TODO should this return a 500?
		r.debugLog("handleCallbackEvent(): failed to read request body, error: %s", err)
		return
	}

	if err := r.handleEvent(string(body[:])); err != nil {
		// TODO should this return a 500?
		r.debugLog("handleCallbackEvent(): failed to handle event: %v", err)
	}
}
