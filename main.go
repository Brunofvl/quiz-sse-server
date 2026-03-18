package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	SupabaseURL   string
	SupabaseKey   string
	Port          string
	CORSOrigin    string
	PollInterval  time.Duration
	HTTPTimeout   time.Duration
	PingInterval  time.Duration
	ShutdownAfter time.Duration
}

type GameSession struct {
	ID                   string     `json:"id"`
	Status               string     `json:"status"`
	CurrentQuestionID    *string    `json:"current_question_id"`
	CurrentQuestionIndex int        `json:"current_question_index"`
	QuestionStartedAt    *time.Time `json:"question_started_at"`
	CreatedAt            *time.Time `json:"created_at"`
	FinishedAt           *time.Time `json:"finished_at"`
}

type Question struct {
	ID             string   `json:"id"`
	Text           string   `json:"text"`
	Options        []string `json:"options"`
	CorrectIndex   int      `json:"correct_index"`
	OrderIndex     int      `json:"order_index"`
	VideoURLViktor *string  `json:"video_url_viktor"`
	VideoURLLucas  *string  `json:"video_url_lucas"`
}

type Answer struct {
	UserID      string `json:"user_id"`
	Team        string `json:"team"`
	IsCorrect   bool   `json:"is_correct"`
	QuestionID  string `json:"question_id"`
	SessionID   string `json:"session_id"`
	ChosenIndex int    `json:"chosen_index"`
}

type QuestionResult struct {
	ID            string  `json:"id"`
	SessionID     string  `json:"session_id"`
	QuestionID    string  `json:"question_id"`
	ViktorCorrect int     `json:"viktor_correct"`
	ViktorTotal   int     `json:"viktor_total"`
	ViktorPct     float64 `json:"viktor_pct"`
	LucasCorrect  int     `json:"lucas_correct"`
	LucasTotal    int     `json:"lucas_total"`
	LucasPct      float64 `json:"lucas_pct"`
	WinnerTeam    *string `json:"winner_team"`
}

type LeaderboardEntry struct {
	SessionID      string `json:"session_id"`
	QuestionID     string `json:"question_id"`
	Name           string `json:"name"`
	Team           string `json:"team"`
	ResponseTimeMS int    `json:"response_time_ms"`
	IsCorrect      bool   `json:"is_correct"`
	SpeedRank      int    `json:"speed_rank"`
}

type SessionPlayer struct {
	SessionID string     `json:"session_id"`
	UserID    string     `json:"user_id"`
	Team      string     `json:"team"`
	JoinedAt  *time.Time `json:"joined_at"`
	IsOnline  bool       `json:"is_online"`
}

type AnswerStats struct {
	TotalPlayers     int     `json:"total_players"`
	AnsweredCount    int     `json:"answered_count"`
	AnsweredPct      float64 `json:"answered_pct"`
	ViktorAnswered   int     `json:"viktor_answered"`
	LucasAnswered    int     `json:"lucas_answered"`
	ViktorCorrectPct float64 `json:"viktor_correct_pct"`
	LucasCorrectPct  float64 `json:"lucas_correct_pct"`
}

type GameState struct {
	Session         *GameSession       `json:"session"`
	CurrentQuestion *Question          `json:"current_question"`
	AnswerStats     *AnswerStats       `json:"answer_stats"`
	QuestionResult  *QuestionResult    `json:"question_result"`
	Leaderboard     []LeaderboardEntry `json:"leaderboard"`
	Players         []SessionPlayer    `json:"players"`
	Timestamp       time.Time          `json:"timestamp"`
}

type SupabaseClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

type StateStore struct {
	mu       sync.RWMutex
	state    *GameState
	lastPoll time.Time
}

type SSEMessage struct {
	Event string
	Data  []byte
}

type SSEClient struct {
	id int
	ch chan SSEMessage
}

type Hub struct {
	mu      sync.RWMutex
	clients map[int]*SSEClient
	nextID  int
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[int]*SSEClient),
	}
}

func (h *Hub) Register() *SSEClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	client := &SSEClient{
		id: h.nextID,
		ch: make(chan SSEMessage, 16),
	}
	h.clients[client.id] = client
	log.Printf("[hub] client connected id=%d total=%d", client.id, len(h.clients))
	return client
}

func (h *Hub) Unregister(client *SSEClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[client.id]; !ok {
		return
	}
	delete(h.clients, client.id)
	close(client.ch)
	log.Printf("[hub] client disconnected id=%d total=%d", client.id, len(h.clients))
}

func (h *Hub) Broadcast(msg SSEMessage) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, client := range h.clients {
		select {
		case client.ch <- msg:
		default:
			select {
			case <-client.ch:
			default:
			}
			select {
			case client.ch <- msg:
			default:
			}
		}
	}
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (s *StateStore) SetState(state *GameState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.lastPoll = time.Now().UTC()
}

func (s *StateStore) GetState() *GameState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state == nil {
		return nil
	}
	copyState := *s.state
	return &copyState
}

func (s *StateStore) LastPoll() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPoll
}

func main() {
	if err := loadDotEnv(".env"); err != nil {
		log.Printf("[env] unable to load .env: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[env] invalid config: %v", err)
	}

	hub := NewHub()
	stateStore := &StateStore{}
	supabase := &SupabaseClient{
		baseURL: strings.TrimRight(cfg.SupabaseURL, "/"),
		apiKey:  cfg.SupabaseKey,
		client: &http.Client{
			Timeout: cfg.HTTPTimeout,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go startPolling(ctx, supabase, stateStore, hub, cfg.PollInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/events", eventsHandler(cfg, hub, stateStore))
	mux.HandleFunc("/health", healthHandler(hub, stateStore))

	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: corsMiddleware(cfg.CORSOrigin, mux),
	}

	go func() {
		log.Printf("[http] listening on :%s", cfg.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[http] server error: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	log.Printf("[http] signal received: %s", sig.String())
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownAfter)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[http] graceful shutdown error: %v", err)
	}
	log.Printf("[http] server stopped")
}

func loadConfig() (*Config, error) {
	supabaseURL := strings.TrimSpace(os.Getenv("SUPABASE_URL"))
	supabaseKey := strings.TrimSpace(os.Getenv("SUPABASE_ANON_KEY"))
	if supabaseURL == "" || supabaseKey == "" {
		return nil, errors.New("SUPABASE_URL and SUPABASE_ANON_KEY are required")
	}

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	corsOrigin := strings.TrimSpace(os.Getenv("CORS_ORIGIN"))
	if corsOrigin == "" {
		corsOrigin = "*"
	}

	pollInterval := 1000
	if raw := strings.TrimSpace(os.Getenv("POLL_INTERVAL_MS")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return nil, errors.New("POLL_INTERVAL_MS must be a positive integer")
		}
		pollInterval = value
	}

	return &Config{
		SupabaseURL:   supabaseURL,
		SupabaseKey:   supabaseKey,
		Port:          port,
		CORSOrigin:    corsOrigin,
		PollInterval:  time.Duration(pollInterval) * time.Millisecond,
		HTTPTimeout:   4 * time.Second,
		PingInterval:  15 * time.Second,
		ShutdownAfter: 8 * time.Second,
	}, nil
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

func corsMiddleware(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Vary", "Origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func eventsHandler(cfg *Config, hub *Hub, stateStore *StateStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		client := hub.Register()
		defer hub.Unregister(client)

		initial := stateStore.GetState()
		if initial == nil {
			initial = &GameState{
				Session:     nil,
				Leaderboard: []LeaderboardEntry{},
				Players:     []SessionPlayer{},
				Timestamp:   time.Now().UTC(),
			}
		}

		initialData, err := json.Marshal(initial)
		if err == nil {
			client.ch <- SSEMessage{
				Event: "state_update",
				Data:  initialData,
			}
		}

		ctx := r.Context()
		done := make(chan struct{})

		go func() {
			defer close(done)
			ticker := time.NewTicker(cfg.PingInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-client.ch:
					if !ok {
						return
					}
					if err := writeSSE(w, flusher, msg.Event, msg.Data); err != nil {
						log.Printf("[sse] write error client=%d err=%v", client.id, err)
						return
					}
				case t := <-ticker.C:
					payload, _ := json.Marshal(map[string]any{
						"timestamp": t.UTC(),
					})
					if err := writeSSE(w, flusher, "ping", payload); err != nil {
						log.Printf("[sse] ping error client=%d err=%v", client.id, err)
						return
					}
				}
			}
		}()

		<-ctx.Done()
		<-done
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, payload []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func healthHandler(hub *Hub, stateStore *StateStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"clients":   hub.Count(),
			"last_poll": stateStore.LastPoll(),
		})
	}
}

func startPolling(ctx context.Context, sb *SupabaseClient, stateStore *StateStore, hub *Hub, interval time.Duration) {
	pollOnce := func() {
		state, err := buildGameState(ctx, sb)
		if err != nil {
			log.Printf("[poll] failed: %v", err)
			return
		}
		stateStore.SetState(state)
		payload, err := json.Marshal(state)
		if err != nil {
			log.Printf("[poll] marshal error: %v", err)
			return
		}
		hub.Broadcast(SSEMessage{
			Event: "state_update",
			Data:  payload,
		})
	}

	pollOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[poll] stopped")
			return
		case <-ticker.C:
			pollOnce()
		}
	}
}

func buildGameState(ctx context.Context, sb *SupabaseClient) (*GameState, error) {
	session, err := sb.FetchActiveSession(ctx)
	if err != nil {
		return nil, err
	}

	state := &GameState{
		Session:     session,
		Leaderboard: []LeaderboardEntry{},
		Players:     []SessionPlayer{},
		Timestamp:   time.Now().UTC(),
	}

	if session == nil {
		return state, nil
	}

	var wg sync.WaitGroup
	var question *Question
	var answers []Answer
	var result *QuestionResult
	var leaderboard []LeaderboardEntry
	var players []SessionPlayer

	questionID := ""
	if session.CurrentQuestionID != nil {
		questionID = *session.CurrentQuestionID
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if questionID == "" {
			return
		}
		q, qErr := sb.FetchQuestionByID(ctx, questionID)
		if qErr != nil {
			log.Printf("[poll] question fetch error: %v", qErr)
			return
		}
		question = q
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if questionID == "" {
			return
		}
		items, aErr := sb.FetchAnswers(ctx, session.ID, questionID)
		if aErr != nil {
			log.Printf("[poll] answers fetch error: %v", aErr)
			return
		}
		answers = items
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if questionID == "" {
			return
		}
		res, rErr := sb.FetchQuestionResult(ctx, session.ID, questionID)
		if rErr != nil {
			log.Printf("[poll] result fetch error: %v", rErr)
			return
		}
		result = res
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if questionID == "" {
			return
		}
		items, lErr := sb.FetchLeaderboard(ctx, session.ID, questionID)
		if lErr != nil {
			log.Printf("[poll] leaderboard fetch error: %v", lErr)
			return
		}
		leaderboard = items
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		items, pErr := sb.FetchOnlinePlayers(ctx, session.ID)
		if pErr != nil {
			log.Printf("[poll] players fetch error: %v", pErr)
			return
		}
		players = items
	}()

	wg.Wait()

	state.CurrentQuestion = question
	state.QuestionResult = result
	if leaderboard != nil {
		state.Leaderboard = leaderboard
	}
	if players != nil {
		state.Players = players
	}
	state.AnswerStats = computeAnswerStats(answers, state.Players)

	return state, nil
}

func computeAnswerStats(answers []Answer, players []SessionPlayer) *AnswerStats {
	unique := make(map[string]Answer)
	for _, answer := range answers {
		if answer.UserID == "" {
			continue
		}
		if _, exists := unique[answer.UserID]; !exists {
			unique[answer.UserID] = answer
		}
	}

	var viktorAnswered int
	var lucasAnswered int
	var viktorCorrect int
	var lucasCorrect int

	for _, answer := range unique {
		switch answer.Team {
		case "viktor":
			viktorAnswered++
			if answer.IsCorrect {
				viktorCorrect++
			}
		case "lucas":
			lucasAnswered++
			if answer.IsCorrect {
				lucasCorrect++
			}
		}
	}

	answeredCount := len(unique)
	totalPlayers := len(players)

	answeredPct := 0.0
	if totalPlayers > 0 {
		answeredPct = round1(float64(answeredCount) * 100 / float64(totalPlayers))
	}

	viktorCorrectPct := 0.0
	if viktorAnswered > 0 {
		viktorCorrectPct = round1(float64(viktorCorrect) * 100 / float64(viktorAnswered))
	}

	lucasCorrectPct := 0.0
	if lucasAnswered > 0 {
		lucasCorrectPct = round1(float64(lucasCorrect) * 100 / float64(lucasAnswered))
	}

	return &AnswerStats{
		TotalPlayers:     totalPlayers,
		AnsweredCount:    answeredCount,
		AnsweredPct:      answeredPct,
		ViktorAnswered:   viktorAnswered,
		LucasAnswered:    lucasAnswered,
		ViktorCorrectPct: viktorCorrectPct,
		LucasCorrectPct:  lucasCorrectPct,
	}
}

func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

func (s *SupabaseClient) FetchActiveSession(ctx context.Context) (*GameSession, error) {
	params := url.Values{}
	params.Set("select", "id,status,current_question_id,current_question_index,question_started_at,created_at,finished_at")
	params.Set("status", "neq.finished")
	params.Set("order", "created_at.desc")
	params.Set("limit", "1")

	var sessions []GameSession
	if err := s.get(ctx, "game_sessions", params, &sessions); err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	return &sessions[0], nil
}

func (s *SupabaseClient) FetchQuestionByID(ctx context.Context, questionID string) (*Question, error) {
	params := url.Values{}
	params.Set("select", "id,text,options,correct_index,order_index,video_url_viktor,video_url_lucas")
	params.Set("id", "eq."+questionID)
	params.Set("limit", "1")

	var questions []Question
	if err := s.get(ctx, "questions", params, &questions); err != nil {
		return nil, err
	}
	if len(questions) == 0 {
		return nil, nil
	}
	return &questions[0], nil
}

func (s *SupabaseClient) FetchAnswers(ctx context.Context, sessionID, questionID string) ([]Answer, error) {
	params := url.Values{}
	params.Set("select", "user_id,team,is_correct,question_id,session_id,chosen_index")
	params.Set("session_id", "eq."+sessionID)
	params.Set("question_id", "eq."+questionID)

	var answers []Answer
	if err := s.get(ctx, "answers", params, &answers); err != nil {
		return nil, err
	}
	return answers, nil
}

func (s *SupabaseClient) FetchQuestionResult(ctx context.Context, sessionID, questionID string) (*QuestionResult, error) {
	params := url.Values{}
	params.Set("select", "id,session_id,question_id,viktor_correct,viktor_total,viktor_pct,lucas_correct,lucas_total,lucas_pct,winner_team")
	params.Set("session_id", "eq."+sessionID)
	params.Set("question_id", "eq."+questionID)
	params.Set("limit", "1")

	var items []QuestionResult
	if err := s.get(ctx, "question_results", params, &items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &items[0], nil
}

func (s *SupabaseClient) FetchLeaderboard(ctx context.Context, sessionID, questionID string) ([]LeaderboardEntry, error) {
	params := url.Values{}
	params.Set("select", "session_id,question_id,name,team,response_time_ms,is_correct,speed_rank")
	params.Set("session_id", "eq."+sessionID)
	params.Set("question_id", "eq."+questionID)
	params.Set("order", "speed_rank.asc")
	params.Set("limit", "5")

	var items []LeaderboardEntry
	if err := s.get(ctx, "leaderboard", params, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *SupabaseClient) FetchOnlinePlayers(ctx context.Context, sessionID string) ([]SessionPlayer, error) {
	params := url.Values{}
	params.Set("select", "session_id,user_id,team,joined_at,is_online")
	params.Set("session_id", "eq."+sessionID)
	params.Set("is_online", "eq.true")
	params.Set("order", "joined_at.asc")

	var players []SessionPlayer
	if err := s.get(ctx, "session_players", params, &players); err != nil {
		return nil, err
	}
	return players, nil
}

func (s *SupabaseClient) get(ctx context.Context, resource string, params url.Values, target any) error {
	endpoint := fmt.Sprintf("%s/rest/v1/%s", s.baseURL, resource)
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", s.apiKey)
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("supabase status=%d resource=%s", resp.StatusCode, resource)
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return err
	}
	return nil
}
