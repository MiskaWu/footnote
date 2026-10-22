// Package web 是本機測試頁：在瀏覽器裡扮演聊天平台（platform=web），不需要 Discord token。
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/miskawu/footnote/internal/bot"
	"github.com/miskawu/footnote/internal/store"
)

//go:embed index.html
var indexHTML []byte

const Platform = "web"

type Server struct {
	Handler *bot.Handler
	Store   *store.Store
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("POST /api/ask", s.ask)
	mux.HandleFunc("POST /api/feedback", s.feedback)
	mux.HandleFunc("GET /api/notices", s.notices)
	mux.HandleFunc("GET /api/gaps", s.gaps)
}

func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Thread, User, Question string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Question == "" || req.Thread == "" {
		http.Error(w, "需要 thread、user、question", http.StatusBadRequest)
		return
	}
	if req.User == "" {
		req.User = "tester"
	}
	rep, err := s.Handler.OnQuestion(r.Context(), bot.Incoming{
		Platform: Platform, ChannelRef: "web", ThreadRef: req.Thread, UserRef: req.User, Text: req.Question,
	}, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AskID int64 `json:"ask_id"`
		User  string
		Vote  int
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AskID == 0 || (req.Vote != 1 && req.Vote != -1) {
		http.Error(w, "需要 ask_id、vote（1 或 -1）", http.StatusBadRequest)
		return
	}
	msg, err := s.Handler.OnFeedback(r.Context(), req.AskID, req.User, req.Vote)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"text": msg})
}

func (s *Server) notices(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	list, err := s.Store.ThreadNotices(r.Context(), Platform, r.URL.Query().Get("thread"), after)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func (s *Server) gaps(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListGaps(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// Sender：測試頁直接從資料庫讀通知，所以「送出」只是標記為已送。
type Sender struct{}

func (Sender) Platform() string                               { return Platform }
func (Sender) Send(ctx context.Context, n store.Notice) error { return nil }
