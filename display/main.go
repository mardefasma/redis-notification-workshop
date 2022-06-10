package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
)

func main() {
	clusterIps, err := net.LookupIP("127.0.0.1")
	if err != nil {
		fmt.Println(err)
		return
	}

	wg := sync.WaitGroup{}
	wg.Add(len(clusterIps))

	ctx, cancel := context.WithCancel(context.Background())

	for _, ip := range clusterIps {
		fmt.Printf("Found cluster: %s\n", ip.String())
	}

	onlineUsername := make(map[string]bool)
	for _, redisClusterIp := range clusterIps {
		go func(redisClusterIp net.IP) {
			redisServerAddr := fmt.Sprintf("%s:6379", redisClusterIp.String())

			c, err := redis.Dial("tcp", redisServerAddr)
			if err != nil {
				fmt.Println(err)
			}

			defer func(c redis.Conn) {
				_ = c.Close()
			}(c)

			err = listenPubSubChannels(ctx, redisServerAddr, func(channel string, message []byte) error {
				// fmt.Printf("channel: %s, message: %s\n", channel, message)
				// For the purpose of this example, cancel the listener's context
				// after receiving last message sent by publish().
				if string(message) == "goodbye" {
					cancel()
				}

				currentUsername := channel[strings.LastIndex(channel, ":")+1:]

				if string(message) == "set" {
					found := onlineUsername[currentUsername]
					if !found || !onlineUsername[currentUsername] {
						onlineUsername[currentUsername] = true
						printSomeoneOnline(currentUsername)
					}

					if strings.Contains(channel, "message") {
						messageValue, err := getMessageValue(currentUsername, c)
						if err != nil {
							fmt.Println(err)
						}

						printMessage(currentUsername, messageValue)
					}
				}

				if string(message) == "expired" {
					found := onlineUsername[currentUsername]
					if found && onlineUsername[currentUsername] {
						onlineUsername[currentUsername] = false
						printSomeoneOffline(currentUsername)
					}
				}

				// TODO: print online/offline notification and message from all users
				return nil
			}, "__keyspace@*:message:*", "__keyspace@*:online:*")

			if err != nil {
				fmt.Println(err)
				// TODO: is it possible to not just stop the app on error?
			}

			wg.Done()
		}(redisClusterIp)
	}
	wg.Wait()
}

func printSomeoneOnline(username string) {
	fmt.Printf("%s is online\n", username)
}

func printSomeoneOffline(username string) {
	fmt.Printf("%s went offline\n", username)
}

func printMessage(senderUsername string, message string) {
	fmt.Printf("%s: %s\n", senderUsername, message)
}

// listenPubSubChannels listens for messages on Redis pubsub channels. The
// onStart function is called after the channels are subscribed. The onMessage
// function is called for each message.
func listenPubSubChannels(ctx context.Context, redisServerAddr string, onMessage func(channel string, data []byte) error, channels ...string) error {
	// A ping is set to the server with this period to test for the health of
	// the connection and server.
	const healthCheckPeriod = time.Minute

	fmt.Printf("Connecting to %s...\n", redisServerAddr)

	c, err := redis.Dial("tcp", redisServerAddr)
	if err != nil {
		return err
	}

	defer func(c redis.Conn) {
		_ = c.Close()
	}(c)

	//if _, err := c.Do("AUTH", password); err != nil {
	//	return err
	//}

	psc := redis.PubSubConn{Conn: c}

	if err := psc.PSubscribe(redis.Args{}.AddFlat(channels)...); err != nil {
		return err
	}

	done := make(chan error, 1)

	// Start a goroutine to receive notifications from the server.
	go func() {
		for {
			pscrec := psc.Receive()
			switch n := pscrec.(type) {
			case error:
				done <- n
				return
			case redis.Message:
				if err := onMessage(n.Channel, n.Data); err != nil {
					done <- err
					return
				}
			case redis.Subscription:
				switch n.Count {
				case len(channels):
					fmt.Printf("Subscribed to %s\n", redisServerAddr)
					// Notify application when all channels are subscribed.
				case 0:
					// Return from the goroutine when all channels are unsubscribed.
					done <- nil
					return
				}
			}
		}
	}()

	ticker := time.NewTicker(healthCheckPeriod)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ticker.C:
			// Send ping to test health of connection and server. If
			// corresponding pong is not received, then receive on the
			// connection will timeout and the receive goroutine will exit.
			if err = psc.Ping(""); err != nil {
				break loop
			}
		case <-ctx.Done():
			break loop
		case err := <-done:
			// Return error from the receive goroutine.
			return err
		}
	}

	// Signal the receiving goroutine to exit by unsubscribing from all channels.
	if err := psc.Unsubscribe(); err != nil {
		return err
	}

	// Wait for goroutine to complete.
	return <-done
}

func getMessageValue(senderUsername string, c redis.Conn) (message string, err error) {
	message, err = redis.String(c.Do("GET", fmt.Sprintf("message:%s", senderUsername)))
	if err != nil {
		return "", err
	}

	return message, nil
}
