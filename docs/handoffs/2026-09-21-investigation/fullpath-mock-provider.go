package main

import (
 "encoding/json"
 "log"
 "net/http"
 "github.com/gorilla/websocket"
)

// An isolated provider fixture: synthetic PCM is acknowledged immediately.
// It neither transcribes audio nor contacts an external supplier.
func main() {
 mux:=http.NewServeMux()
 mux.HandleFunc("/v1/api_keys",func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("Content-Type","application/json")
  _,_=w.Write([]byte(`{"key_value":"fixture-only"}`))
 })
 mux.HandleFunc("/v2",func(w http.ResponseWriter,r *http.Request){
  upgrader:=websocket.Upgrader{CheckOrigin:func(*http.Request)bool{return true}}
  c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
  seq:=0
  for {kind,b,e:=c.ReadMessage();if e!=nil{return}
   if kind==websocket.BinaryMessage {seq++;if e=c.WriteJSON(map[string]any{"message":"AudioAdded","seq_no":seq});e!=nil{return};continue}
   var request map[string]any;if json.Unmarshal(b,&request)!=nil{return}
   switch request["message"] {
   case "StartRecognition": e=c.WriteJSON(map[string]any{"message":"RecognitionStarted","id":"fixture"})
   case "EndOfStream": _=c.WriteJSON(map[string]any{"message":"EndOfTranscript"});return
   }
   if e!=nil{return}
  }
 })
 log.Fatal(http.ListenAndServeTLS(":443","/fixture/cert.pem","/fixture/key.pem",mux))
}
