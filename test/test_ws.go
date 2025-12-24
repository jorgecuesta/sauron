// Simple WebSocket test for Sauron
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run test_ws.go <websocket-url>")
		fmt.Println("Example: go run test_ws.go ws://localhost:8081/websocket")
		os.Exit(1)
	}

	wsURL := os.Args[1]
	fmt.Printf("Testing WebSocket connection to: %s\n", wsURL)

	// Connect to WebSocket
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dialer := websocket.DefaultDialer
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		log.Fatalf("❌ Failed to connect: %v (response: %v)", err, resp)
	}
	defer func() { _ = conn.Close() }()

	fmt.Println("✓ WebSocket connection established")
	fmt.Printf("  Response status: %d\n", resp.StatusCode)

	// Send a subscription request (CometBFT format)
	subscribeMsg := `{"jsonrpc":"2.0","method":"subscribe","id":1,"params":{"query":"tm.event='NewBlock'"}}`
	err = conn.WriteMessage(websocket.TextMessage, []byte(subscribeMsg))
	if err != nil {
		log.Fatalf("❌ Failed to send subscribe message: %v", err)
	}
	fmt.Println("✓ Sent subscription request")

	// Set read deadline
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Fatalf("❌ Failed to set read deadline: %v", err)
	}

	// Read response
	_, message, err := conn.ReadMessage()
	if err != nil {
		log.Printf("⚠ Failed to read response (this might be normal if no new blocks): %v", err)
	} else {
		fmt.Println("✓ Received response from WebSocket:")
		fmt.Printf("  %s\n", string(message))
	}

	// Send unsubscribe
	unsubscribeMsg := `{"jsonrpc":"2.0","method":"unsubscribe","id":2,"params":{"query":"tm.event='NewBlock'"}}`
	_ = conn.WriteMessage(websocket.TextMessage, []byte(unsubscribeMsg))

	// Close connection gracefully
	err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		log.Printf("⚠ Error sending close message: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // Wait for close handshake

	fmt.Println("\n✅ WebSocket test completed successfully")
}
