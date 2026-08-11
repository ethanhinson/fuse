package loopserver

// This file is the future home of binding #3's WebSocket transport (the WS
// `conn` implementation) and the thin stateless HTTP replay handler. For now it
// carries only a compile-only proof that the github.com/coder/websocket
// dependency resolves and links; the real transport logic is added by a later
// task in the networked-runtime-binding plan.

import "github.com/coder/websocket"

// _ pins a trivially-used symbol from coder/websocket so the import is exercised
// by the compiler without introducing any runtime behavior yet.
var _ = websocket.MessageText
