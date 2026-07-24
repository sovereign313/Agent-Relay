package relay

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sovereign313/Agent-Relay/internal/agent"
	"github.com/sovereign313/Agent-Relay/internal/config"
	"github.com/sovereign313/Agent-Relay/internal/project"
	"github.com/sovereign313/Agent-Relay/internal/session"
	"github.com/sovereign313/Agent-Relay/internal/store"
	"github.com/sovereign313/Agent-Relay/internal/telegram"
)

type Telegram interface {
	GetUpdates(context.Context, int64) ([]telegram.Update, error)
	Send(context.Context, int64, string) error
	SendKeyboard(context.Context, int64, string, [][]telegram.Button) error
	AnswerCallback(context.Context, string, string) error
}

type Service struct {
	cfg      *config.Config
	log      *slog.Logger
	telegram Telegram
	store    *store.Store
	runners  map[string]agent.Runner
	sessions *session.Manager

	catalogMu sync.RWMutex
	catalog   *project.Catalog

	dispatchMu    sync.Mutex
	dispatched    map[int64]bool
	deliveryMu    sync.Mutex
	outboxWake    chan struct{}
	startedAt     time.Time
	version       string
	agentVersions map[string]string
}

func New(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	client Telegram,
	stateStore *store.Store,
	catalog *project.Catalog,
	runners map[string]agent.Runner,
	version string,
	agentVersions map[string]string,
) *Service {
	service := &Service{
		cfg:           cfg,
		log:           logger,
		telegram:      client,
		store:         stateStore,
		catalog:       catalog,
		runners:       runners,
		dispatched:    make(map[int64]bool),
		outboxWake:    make(chan struct{}, 1),
		startedAt:     time.Now().UTC(),
		version:       version,
		agentVersions: agentVersions,
	}
	service.sessions = session.NewManager(ctx, cfg.QueueSize, service.process)
	return service
}

func (s *Service) Run(ctx context.Context) error {
	defer s.sessions.Close()
	interrupted, err := s.store.ReconcileStartup()
	if err != nil {
		return fmt.Errorf("reconcile persisted state: %w", err)
	}
	for _, job := range interrupted {
		message := store.OutboxMessage{
			ID:        fmt.Sprintf("interrupted:%d", job.ID),
			ChatID:    job.ChatID,
			Text:      fmt.Sprintf("Job %d for %s in %s was interrupted by an Agent Relay restart. Select that project and agent, then use /retry %d to run it again or /clearqueue to discard it.", job.ID, job.AgentName, job.ProjectID, job.ID),
			CreatedAt: time.Now().UTC(),
		}
		if err := s.store.PutOutbox(message); err != nil {
			return fmt.Errorf("persist interrupted-job notice: %w", err)
		}
	}
	go s.runOutbox(ctx)
	s.dispatchPending()

	offset := s.store.UpdateOffset()
	s.log.Info("telegram polling started", "offset", offset)

	for {
		updates, err := s.telegram.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Error("telegram polling failed", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				continue
			}
		}
		for _, update := range updates {
			if err := s.handleUpdate(ctx, update); err != nil {
				return err
			}
			offset = s.store.UpdateOffset()
		}
	}
}

func (s *Service) handleUpdate(ctx context.Context, update telegram.Update) error {
	if update.CallbackQuery != nil {
		return s.handleCallback(update)
	}
	if update.Message == nil {
		_, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
		return err
	}
	message := update.Message
	if !s.cfg.IsAllowedUser(message.From.ID) {
		s.log.Warn("unauthorized Telegram message", "user_id", message.From.ID, "chat_id", message.Chat.ID)
		_, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
		return err
	}
	if *s.cfg.PrivateChatsOnly && message.Chat.Type != "private" {
		s.log.Warn("non-private Telegram message rejected", "user_id", message.From.ID, "chat_id", message.Chat.ID, "chat_type", message.Chat.Type)
		_, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
		return err
	}
	text := strings.TrimSpace(message.Text)
	if text == "" {
		if _, err := s.store.AcceptUpdate(update.UpdateID, nil, nil); err != nil {
			return err
		}
		s.send(message.Chat.ID, "Only text messages are supported.")
		return nil
	}
	if len([]byte(text)) > s.cfg.MaxMessageBytes {
		if _, err := s.store.AcceptUpdate(update.UpdateID, nil, nil); err != nil {
			return err
		}
		s.send(message.Chat.ID, fmt.Sprintf("Message is too large. Limit: %d bytes.", s.cfg.MaxMessageBytes))
		return nil
	}

	if strings.HasPrefix(text, "/") {
		accepted, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
		if err != nil {
			return err
		}
		if !accepted {
			return nil
		}
		s.handleCommand(ctx, message.Chat.ID, text)
		return nil
	}
	return s.acceptJob(update.UpdateID, message.Chat.ID, text)
}

func (s *Service) handleCommand(ctx context.Context, chatID int64, text string) {
	fields := strings.Fields(text)
	command := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0])
	switch command {
	case "/start", "/help":
		s.send(chatID, helpText)
	case "/projects", "/list":
		s.sendProjects(chatID)
	case "/agents":
		s.sendAgents(chatID)
	case "/agent":
		if len(fields) != 2 {
			s.send(chatID, "Usage: /agent <agent-name>")
			return
		}
		s.selectAgent(chatID, fields[1])
	case "/project", "/use":
		if len(fields) != 2 {
			s.send(chatID, "Usage: /project <project-id>")
			return
		}
		s.selectProject(chatID, fields[1])
	case "/sessions":
		s.send(chatID, s.sessionsMessage(chatID))
	case "/queue":
		s.send(chatID, s.queueMessage(chatID))
	case "/clearqueue":
		s.clearQueue(chatID)
	case "/new", "/restart":
		s.newConversation(chatID)
	case "/last":
		s.sendLast(chatID)
	case "/status":
		s.send(chatID, s.statusMessage(chatID))
	case "/cancel":
		s.cancel(chatID)
	case "/cancelall":
		s.cancelAll(chatID)
	case "/retry":
		if len(fields) != 2 {
			s.send(chatID, "Usage: /retry <job-id>")
			return
		}
		jobID, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			s.send(chatID, "Job ID must be a number.")
			return
		}
		s.retryJob(chatID, jobID)
	case "/refresh":
		if err := s.refreshCatalog(); err != nil {
			s.log.Error("project refresh failed", "error", err)
			s.send(chatID, "Project refresh failed: "+safeError(err))
			return
		}
		s.sendProjects(chatID)
	default:
		s.send(chatID, "Unknown command. Use /help.")
	}
}

func (s *Service) handleCallback(update telegram.Update) error {
	callback := update.CallbackQuery
	if callback == nil || callback.Message == nil {
		_, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
		return err
	}
	if !s.cfg.IsAllowedUser(callback.From.ID) {
		s.log.Warn("unauthorized Telegram callback", "user_id", callback.From.ID)
		_, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
		return err
	}
	if *s.cfg.PrivateChatsOnly && callback.Message.Chat.Type != "private" {
		_, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
		return err
	}
	accepted, err := s.store.AcceptUpdate(update.UpdateID, nil, nil)
	if err != nil || !accepted {
		return err
	}
	switch {
	case strings.HasPrefix(callback.Data, "project:"):
		projectID := strings.TrimPrefix(callback.Data, "project:")
		if _, ok := s.getProject(projectID); !ok {
			s.answerCallback(callback.ID, "Project unavailable")
			return nil
		}
		s.selectProject(callback.Message.Chat.ID, projectID)
		s.answerCallback(callback.ID, "Selected "+projectID)
	case strings.HasPrefix(callback.Data, "agent:"):
		agentName := strings.TrimPrefix(callback.Data, "agent:")
		if _, ok := s.runners[agentName]; !ok {
			s.answerCallback(callback.ID, "Agent unavailable")
			return nil
		}
		s.selectAgent(callback.Message.Chat.ID, agentName)
		s.answerCallback(callback.ID, "Selected "+agentName)
	default:
		s.answerCallback(callback.ID, "Unknown action")
	}
	return nil
}

func (s *Service) selectProject(chatID int64, id string) {
	selected, ok := s.getProject(id)
	if !ok {
		s.send(chatID, "Unknown project. Run /projects to list available projects.")
		return
	}
	if err := s.store.SetSelectedProject(chatID, selected.ID); err != nil {
		s.log.Error("persist selected project", "error", err)
		s.send(chatID, "Could not save the selected project.")
		return
	}
	agentName := s.selectedAgent(chatID)
	if conversation, exists := s.store.Conversation(chatID, selected.ID, agentName); exists && conversation.ThreadID != "" {
		s.send(chatID, "Selected "+selected.ID+". Existing "+agentName+" context will be resumed.")
	} else {
		s.send(chatID, "Selected "+selected.ID+". The next message starts a new "+agentName+" context.")
	}
}

func (s *Service) selectAgent(chatID int64, name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if _, ok := s.runners[name]; !ok {
		s.send(chatID, "Unknown agent. Run /agents to list available agents.")
		return
	}
	if err := s.store.SetSelectedAgent(chatID, name); err != nil {
		s.log.Error("persist selected agent", "error_type", fmt.Sprintf("%T", err))
		s.send(chatID, "Could not save the selected agent.")
		return
	}
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		s.send(chatID, "Selected "+name+". Select a project with /projects.")
		return
	}
	if conversation, exists := s.store.Conversation(chatID, projectID, name); exists && conversation.ThreadID != "" {
		s.send(chatID, "Selected "+name+". Existing context for "+projectID+" will be resumed.")
	} else {
		s.send(chatID, "Selected "+name+". The next message starts a new context for "+projectID+".")
	}
}

func (s *Service) newConversation(chatID int64) {
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		s.send(chatID, "Select a project first with /project <project-id>.")
		return
	}
	agentName := s.selectedAgent(chatID)
	key := session.Key{ChatID: chatID, ProjectID: projectID, AgentName: agentName}
	status := s.sessions.Status(key)
	if status.Working || status.Queued > 0 || s.hasPersistedJobs(chatID, projectID, agentName) {
		s.send(chatID, "Cancel the active task and wait for the queue to clear before starting a new context.")
		return
	}
	if conversation, exists := s.store.Conversation(chatID, projectID, agentName); exists && conversation.ThreadID != "" {
		if resetter, ok := s.runners[agentName].(agent.Resetter); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := resetter.Reset(ctx, conversation.ThreadID)
			cancel()
			if err != nil {
				s.log.Warn("reset old agent session failed", "agent", agentName, "project", projectID, "error_type", fmt.Sprintf("%T", err))
			}
		}
	}
	if err := s.store.DeleteConversation(chatID, projectID, agentName); err != nil {
		s.log.Error("delete conversation", "error", err)
		s.send(chatID, "Could not reset the "+agentName+" context.")
		return
	}
	s.send(chatID, "The next message will start a new "+agentName+" context for "+projectID+".")
}

func (s *Service) cancel(chatID int64) {
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		s.send(chatID, "No project is selected.")
		return
	}
	agentName := s.selectedAgent(chatID)
	if !s.sessions.Cancel(session.Key{ChatID: chatID, ProjectID: projectID, AgentName: agentName}) {
		s.send(chatID, agentName+" is not currently working in "+projectID+".")
		return
	}
	if conversation, exists := s.store.Conversation(chatID, projectID, agentName); exists {
		conversation.State = store.StateStopped
		conversation.LastActivity = time.Now().UTC()
		conversation.LastError = "cancellation requested"
		if err := s.store.PutConversation(conversation); err != nil {
			s.log.Error("persist cancellation state", "error_type", fmt.Sprintf("%T", err))
		}
	}
	s.send(chatID, "Cancelling the current "+agentName+" task...")
}

func (s *Service) cancelAll(chatID int64) {
	count := s.sessions.CancelChat(chatID)
	if count == 0 {
		s.send(chatID, "No agent tasks are currently running.")
		return
	}
	s.send(chatID, fmt.Sprintf("Cancelling %d running agent task(s)...", count))
}

func (s *Service) clearQueue(chatID int64) {
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		s.send(chatID, "No project is selected.")
		return
	}
	agentName := s.selectedAgent(chatID)
	key := session.Key{ChatID: chatID, ProjectID: projectID, AgentName: agentName}
	status := s.sessions.Status(key)
	excludeID := int64(-1)
	if status.Working {
		excludeID = status.CurrentID
	}
	removedMemory := s.sessions.ClearQueue(key)
	for _, job := range removedMemory {
		s.markDispatched(job.ID, false)
	}
	removed, err := s.store.ClearJobs(chatID, projectID, agentName, excludeID)
	if err != nil {
		s.log.Error("clear persisted jobs", "error_type", fmt.Sprintf("%T", err))
		s.send(chatID, "Could not clear the persisted queue.")
		return
	}
	for _, id := range removed {
		s.markDispatched(id, false)
	}
	if !status.Working {
		if conversation, exists := s.store.Conversation(chatID, projectID, agentName); exists && conversation.State == store.StateQueued {
			conversation.State = store.StateIdle
			conversation.LastActivity = time.Now().UTC()
			_ = s.store.PutConversation(conversation)
		}
	}
	s.send(chatID, fmt.Sprintf("Cleared %d queued or interrupted %s job(s) from %s.", len(removed), agentName, projectID))
}

func (s *Service) retryJob(chatID, jobID int64) {
	job, ok := s.store.Job(jobID)
	if !ok || job.ChatID != chatID {
		s.send(chatID, "Unknown job ID.")
		return
	}
	if selected := s.store.SelectedProject(chatID); selected != job.ProjectID {
		s.send(chatID, "Select "+job.ProjectID+" before retrying this job.")
		return
	}
	if selected := s.selectedAgent(chatID); selected != job.AgentName {
		s.send(chatID, "Select "+job.AgentName+" with /agent before retrying this job.")
		return
	}
	job, err := s.store.RetryJob(jobID)
	if err != nil {
		s.send(chatID, safeError(err))
		return
	}
	if conversation, exists := s.store.Conversation(chatID, job.ProjectID, job.AgentName); exists {
		conversation.State = store.StateQueued
		conversation.LastActivity = time.Now().UTC()
		conversation.LastError = ""
		if err := s.store.PutConversation(conversation); err != nil {
			s.send(chatID, "Job is pending, but its session state could not be updated.")
		}
	}
	queued, err := s.dispatchJob(job)
	if err != nil {
		s.send(chatID, "Job was marked pending and will run when queue space is available.")
		return
	}
	if queued {
		s.send(chatID, fmt.Sprintf("Job %d was queued.", jobID))
	} else {
		s.send(chatID, fmt.Sprintf("Retrying job %d with %s in %s...", jobID, job.AgentName, job.ProjectID))
	}
}

func (s *Service) sendLast(chatID int64) {
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		s.send(chatID, "No project is selected.")
		return
	}
	agentName := s.selectedAgent(chatID)
	conversation, exists := s.store.Conversation(chatID, projectID, agentName)
	if !exists || conversation.LastResponse == "" {
		s.send(chatID, "No completed "+agentName+" response is stored for "+projectID+".")
		return
	}
	s.send(chatID, conversation.LastResponse)
}

func (s *Service) projectsMessage(chatID int64) string {
	selected := s.store.SelectedProject(chatID)
	projects := s.listProjects()
	if len(projects) == 0 {
		return "No Git projects were discovered beneath the configured roots."
	}
	var lines []string
	lines = append(lines, "Projects:")
	for _, item := range projects {
		marker := "  "
		if item.ID == selected {
			marker = "* "
		}
		lines = append(lines, marker+item.ID+" — "+item.RelativePath)
	}
	return strings.Join(lines, "\n")
}

func (s *Service) sendProjects(chatID int64) {
	message := s.projectsMessage(chatID)
	projects := s.listProjects()
	rows := make([][]telegram.Button, 0, (len(projects)+1)/2)
	var row []telegram.Button
	for _, item := range projects {
		data := "project:" + item.ID
		if len(data) > 64 {
			continue
		}
		row = append(row, telegram.Button{Text: item.ID, Data: data})
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.telegram.SendKeyboard(ctx, chatID, message, rows); err != nil {
		s.log.Error("send project keyboard", "chat_id", chatID, "error_type", fmt.Sprintf("%T", err))
	}
}

func (s *Service) agentsMessage(chatID int64) string {
	selected := s.selectedAgent(chatID)
	names := make([]string, 0, len(s.runners))
	for name := range s.runners {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := []string{"Agents:"}
	for _, name := range names {
		marker := "  "
		if name == selected {
			marker = "* "
		}
		lines = append(lines, marker+name+" — "+s.agentVersions[name])
	}
	return strings.Join(lines, "\n")
}

func (s *Service) sendAgents(chatID int64) {
	names := make([]string, 0, len(s.runners))
	for name := range s.runners {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([][]telegram.Button, 0, (len(names)+1)/2)
	for index := 0; index < len(names); index += 2 {
		row := []telegram.Button{{Text: names[index], Data: "agent:" + names[index]}}
		if index+1 < len(names) {
			row = append(row, telegram.Button{Text: names[index+1], Data: "agent:" + names[index+1]})
		}
		rows = append(rows, row)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.telegram.SendKeyboard(ctx, chatID, s.agentsMessage(chatID), rows); err != nil {
		s.log.Error("send agent keyboard", "chat_id", chatID, "error_type", fmt.Sprintf("%T", err))
	}
}

func (s *Service) sessionsMessage(chatID int64) string {
	conversations := s.store.Conversations(chatID)
	if len(conversations) == 0 {
		return "No agent project sessions have been created."
	}
	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].LastActivity.After(conversations[j].LastActivity)
	})
	lines := []string{"Sessions:"}
	for _, conversation := range conversations {
		thread := "new"
		if conversation.ThreadID != "" {
			thread = shortID(conversation.ThreadID)
		}
		lines = append(lines, fmt.Sprintf("%s — %s — %s — %s", conversation.ProjectID, conversation.AgentName, conversation.State, thread))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) queueMessage(chatID int64) string {
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		return "No project is selected."
	}
	agentName := s.selectedAgent(chatID)
	var jobs []store.PendingJob
	for _, job := range s.store.Jobs() {
		if job.ChatID == chatID && job.ProjectID == projectID && job.AgentName == agentName {
			jobs = append(jobs, job)
		}
	}
	if len(jobs) == 0 {
		return "The " + agentName + " queue for " + projectID + " is empty."
	}
	lines := []string{"Queue for " + projectID + " with " + agentName + ":"}
	for _, job := range jobs {
		preview := strings.Join(strings.Fields(job.Prompt), " ")
		if len(preview) > 72 {
			preview = preview[:72] + "..."
		}
		lines = append(lines, fmt.Sprintf("%d — %s — %s", job.ID, job.State, preview))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) statusMessage(chatID int64) string {
	agentName := s.selectedAgent(chatID)
	runtime := fmt.Sprintf(
		"Agent Relay: %s\nAgent: %s (%s)\nUptime: %s",
		s.version,
		agentName,
		s.agentVersions[agentName],
		formatUptime(time.Since(s.startedAt)),
	)
	projectID := s.store.SelectedProject(chatID)
	if projectID == "" {
		return runtime + "\nProject: not selected"
	}
	selected, ok := s.getProject(projectID)
	if !ok {
		return runtime + "\nProject: " + projectID + "\nState: unavailable"
	}
	conversation, exists := s.store.Conversation(chatID, projectID, agentName)
	status := s.sessions.Status(session.Key{ChatID: chatID, ProjectID: projectID, AgentName: agentName})
	state := store.StateIdle
	thread := "not started"
	if exists {
		state = conversation.State
		if conversation.ThreadID != "" {
			thread = shortID(conversation.ThreadID)
		}
	}
	if status.Working {
		state = store.StateWorking
	} else if status.Queued > 0 {
		state = store.StateQueued
	}
	durableJobs := 0
	for _, job := range s.store.Jobs() {
		if job.ChatID == chatID && job.ProjectID == projectID && job.AgentName == agentName {
			durableJobs++
		}
	}
	return fmt.Sprintf(
		"%s\nProject: %s\nPath: %s\nState: %s\nQueued: %d\nDurable jobs: %d\nThread: %s",
		runtime,
		projectID,
		selected.Path,
		state,
		status.Queued,
		durableJobs,
		thread,
	)
}

func (s *Service) refreshCatalog() error {
	roots := make([]string, 0, len(s.cfg.ProjectRoots))
	for _, root := range s.cfg.ProjectRoots {
		roots = append(roots, s.cfg.ResolvePath(root))
	}
	catalog, err := project.Discover(roots, s.cfg.ProjectAliases, s.cfg.ProjectDiscoveryDepth)
	if err != nil {
		return err
	}
	s.catalogMu.Lock()
	s.catalog = catalog
	s.catalogMu.Unlock()
	s.log.Info("project catalog refreshed", "projects", len(catalog.List()))
	return nil
}

func (s *Service) getProject(id string) (project.Project, bool) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	return s.catalog.Get(id)
}

func (s *Service) listProjects() []project.Project {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	return s.catalog.List()
}

func (s *Service) selectedAgent(chatID int64) string {
	name := s.store.SelectedAgent(chatID)
	if _, ok := s.runners[name]; ok {
		return name
	}
	return s.cfg.DefaultAgent
}
