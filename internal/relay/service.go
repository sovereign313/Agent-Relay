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
	"github.com/sovereign313/Agent-Relay/internal/transport"
)

type Telegram interface {
	transport.Sender
	GetUpdates(context.Context, int64) ([]telegram.Update, error)
}

type Service struct {
	cfg      *config.Config
	log      *slog.Logger
	telegram Telegram
	senders  map[string]transport.Sender
	sources  []transport.Source
	store    *store.Store
	runners  map[string]agent.Runner
	sessions *session.Manager

	catalogMu sync.RWMutex
	catalog   *project.Catalog

	dispatchMu    sync.Mutex
	dispatched    map[int64]bool
	deliveryMu    sync.Mutex
	commandMu     sync.Mutex
	replyMu       sync.Mutex
	replies       map[transport.Address]string
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
	sources ...transport.Source,
) *Service {
	senders := make(map[string]transport.Sender)
	if client != nil {
		senders[client.Name()] = client
	}
	for _, source := range sources {
		senders[source.Name()] = source
	}
	service := &Service{
		cfg:           cfg,
		log:           logger,
		telegram:      client,
		senders:       senders,
		sources:       sources,
		store:         stateStore,
		catalog:       catalog,
		runners:       runners,
		dispatched:    make(map[int64]bool),
		replies:       make(map[transport.Address]string),
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
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted, err := s.store.ReconcileStartup()
	if err != nil {
		return fmt.Errorf("reconcile persisted state: %w", err)
	}
	for _, job := range interrupted {
		message := store.OutboxMessage{
			ID:             fmt.Sprintf("interrupted:%d", job.ID),
			Transport:      job.Transport,
			ConversationID: job.ConversationID,
			Text:           fmt.Sprintf("Job %s for %s in %s was interrupted by an Agent Relay restart. Select that project and agent, then use /retry %s to run it again or /clearqueue to discard it.", jobReference(job.ID), job.AgentName, job.ProjectID, jobReference(job.ID)),
			EditMessageID:  job.StatusMessageID,
			Buttons:        retryButtons(job.ID),
			CreatedAt:      time.Now().UTC(),
		}
		if err := s.store.PutOutbox(message); err != nil {
			return fmt.Errorf("persist interrupted-job notice: %w", err)
		}
	}
	go s.runOutbox(runContext)
	s.dispatchPending()
	transportCount := len(s.sources)
	if s.telegram != nil {
		transportCount++
	}
	transportErrors := make(chan error, transportCount)
	for _, source := range s.sources {
		source := source
		go func() {
			err := source.Run(runContext, s.handleInbound)
			if err == nil && runContext.Err() == nil {
				err = fmt.Errorf("%s transport stopped unexpectedly", source.Name())
			}
			transportErrors <- err
		}()
	}
	if s.telegram != nil {
		go func() {
			transportErrors <- s.runTelegram(runContext)
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-transportErrors:
		if err == nil && ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func (s *Service) runTelegram(ctx context.Context) error {
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
	address := transport.Address{Transport: transport.Telegram, ConversationID: strconv.FormatInt(message.Chat.ID, 10)}
	if text == "" {
		if _, err := s.store.AcceptUpdate(update.UpdateID, nil, nil); err != nil {
			return err
		}
		s.send(address, "Only text messages are supported.")
		return nil
	}
	if len([]byte(text)) > s.cfg.MaxMessageBytes {
		if _, err := s.store.AcceptUpdate(update.UpdateID, nil, nil); err != nil {
			return err
		}
		s.send(address, fmt.Sprintf("Message is too large. Limit: %d bytes.", s.cfg.MaxMessageBytes))
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
		s.handleCommand(ctx, address, text)
		return nil
	}
	return s.acceptJob(transport.Inbound{
		EventID:  fmt.Sprintf("telegram:%d", update.UpdateID),
		Sequence: update.UpdateID,
		Address:  address,
		UserID:   strconv.FormatInt(message.From.ID, 10),
		Private:  message.Chat.Type == "private",
		Text:     text,
	})
}

func (s *Service) handleInbound(ctx context.Context, inbound transport.Inbound) error {
	if !inbound.Address.Valid() || inbound.EventID == "" {
		return fmt.Errorf("invalid inbound transport event")
	}
	if inbound.Autocomplete != nil {
		return s.handleAutocomplete(ctx, inbound.Address, inbound.Autocomplete)
	}
	text := strings.TrimSpace(inbound.Text)
	if len([]byte(text)) > s.cfg.MaxMessageBytes {
		if _, err := s.acceptInbound(inbound, nil, nil); err != nil {
			return err
		}
		s.send(inbound.Address, fmt.Sprintf("Message is too large. Limit: %d bytes.", s.cfg.MaxMessageBytes))
		return nil
	}
	if inbound.Action != nil {
		accepted, err := s.acceptInbound(inbound, nil, nil)
		if err != nil || !accepted {
			return err
		}
		s.withResponse(inbound, func() {
			s.handleAction(inbound.Address, inbound.Action)
		})
		return nil
	}
	if text == "" {
		_, err := s.acceptInbound(inbound, nil, nil)
		return err
	}
	if strings.HasPrefix(text, "/") {
		accepted, err := s.acceptInbound(inbound, nil, nil)
		if err != nil || !accepted {
			return err
		}
		s.withResponse(inbound, func() {
			s.handleCommand(ctx, inbound.Address, text)
		})
		return nil
	}
	return s.acceptJob(inbound)
}

func (s *Service) withResponse(inbound transport.Inbound, handle func()) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if inbound.ResponseID != "" {
		s.replyMu.Lock()
		s.replies[inbound.Address] = inbound.ResponseID
		s.replyMu.Unlock()
		defer func() {
			s.replyMu.Lock()
			delete(s.replies, inbound.Address)
			s.replyMu.Unlock()
		}()
	}
	handle()
}

func (s *Service) handleAutocomplete(ctx context.Context, address transport.Address, request *transport.Autocomplete) error {
	sender := s.sender(address)
	responder, ok := sender.(transport.AutocompleteResponder)
	if !ok {
		return transport.ErrUnsupported
	}
	query := strings.ToLower(strings.TrimSpace(request.Query))
	var choices []transport.Choice
	switch request.Command {
	case "project":
		for _, item := range s.listProjects() {
			if query != "" && !strings.Contains(strings.ToLower(item.ID+" "+item.RelativePath), query) {
				continue
			}
			choices = append(choices, transport.Choice{Name: item.ID, Value: item.ID})
			if len(choices) == 25 {
				break
			}
		}
	case "agent":
		names := make([]string, 0, len(s.runners))
		for name := range s.runners {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if query == "" || strings.Contains(name, query) {
				choices = append(choices, transport.Choice{Name: name, Value: name})
			}
		}
	}
	return responder.AnswerAutocomplete(ctx, request.ID, choices)
}

func (s *Service) acceptInbound(inbound transport.Inbound, job *store.PendingJob, conversation *store.Conversation) (bool, error) {
	if inbound.Address.Transport == transport.Telegram {
		return s.store.AcceptUpdate(inbound.Sequence, job, conversation)
	}
	return s.store.AcceptEvent(inbound.EventID, job, conversation)
}

func (s *Service) handleCommand(ctx context.Context, address transport.Address, text string) {
	fields := strings.Fields(text)
	command := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0])
	switch command {
	case "/start", "/help":
		s.send(address, helpText)
	case "/projects", "/list":
		s.sendProjects(address)
	case "/agents":
		s.sendAgents(address)
	case "/agent":
		if len(fields) != 2 {
			s.send(address, "Usage: /agent <agent-name>")
			return
		}
		s.selectAgent(address, fields[1])
	case "/project", "/use":
		if len(fields) != 2 {
			s.send(address, "Usage: /project <project-id>")
			return
		}
		s.selectProject(address, fields[1])
	case "/sessions":
		s.send(address, s.sessionsMessage(address))
	case "/queue":
		s.send(address, s.queueMessage(address))
	case "/clearqueue":
		s.clearQueue(address)
	case "/new", "/restart":
		s.newConversation(address)
	case "/last":
		s.sendLast(address)
	case "/status":
		s.send(address, s.statusMessage(address))
	case "/cancel":
		s.cancel(address)
	case "/cancelall":
		s.cancelAll(address)
	case "/retry":
		if len(fields) != 2 {
			s.send(address, "Usage: /retry <job-id>")
			return
		}
		jobID, err := parseJobReference(fields[1])
		if err != nil {
			s.send(address, "Job ID must be a number.")
			return
		}
		s.retryJob(address, jobID)
	case "/refresh":
		if err := s.refreshCatalog(); err != nil {
			s.log.Error("project refresh failed", "error", err)
			s.send(address, "Project refresh failed: "+safeError(err))
			return
		}
		s.sendProjects(address)
	default:
		s.send(address, "Unknown command. Use /help.")
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
	address := transport.Address{Transport: transport.Telegram, ConversationID: strconv.FormatInt(callback.Message.Chat.ID, 10)}
	s.handleAction(address, &transport.Action{ID: callback.ID, Data: callback.Data})
	return nil
}

func (s *Service) handleAction(address transport.Address, action *transport.Action) {
	switch {
	case action.Data == "clearqueue":
		s.clearQueue(address)
		s.answerAction(address.Transport, action.ID, "Queue cleared")
	case strings.HasPrefix(action.Data, "canceljob:"):
		jobID, err := strconv.ParseInt(strings.TrimPrefix(action.Data, "canceljob:"), 10, 64)
		if err != nil {
			s.answerAction(address.Transport, action.ID, "Invalid job")
			return
		}
		s.cancelJob(address, jobID)
		s.answerAction(address.Transport, action.ID, "Cancellation requested")
	case strings.HasPrefix(action.Data, "retryjob:"):
		jobID, err := strconv.ParseInt(strings.TrimPrefix(action.Data, "retryjob:"), 10, 64)
		if err != nil {
			s.answerAction(address.Transport, action.ID, "Invalid job")
			return
		}
		s.retryJob(address, jobID)
		s.answerAction(address.Transport, action.ID, "Retry requested")
	case strings.HasPrefix(action.Data, "project:"):
		projectID := strings.TrimPrefix(action.Data, "project:")
		if _, ok := s.getProject(projectID); !ok {
			s.answerAction(address.Transport, action.ID, "Project unavailable")
			return
		}
		s.selectProject(address, projectID)
		s.answerAction(address.Transport, action.ID, "Selected "+projectID)
	case strings.HasPrefix(action.Data, "agent:"):
		agentName := strings.TrimPrefix(action.Data, "agent:")
		if _, ok := s.runners[agentName]; !ok {
			s.answerAction(address.Transport, action.ID, "Agent unavailable")
			return
		}
		s.selectAgent(address, agentName)
		s.answerAction(address.Transport, action.ID, "Selected "+agentName)
	default:
		s.answerAction(address.Transport, action.ID, "Unknown action")
	}
}

func (s *Service) cancelJob(address transport.Address, jobID int64) {
	job, ok := s.store.Job(jobID)
	if !ok || job.Address() != address {
		s.send(address, "That task is no longer active.")
		return
	}
	key := session.Key{Address: address, ProjectID: job.ProjectID, AgentName: job.AgentName}
	status := s.sessions.Status(key)
	if !status.Working || status.CurrentID != jobID || !s.sessions.Cancel(key) {
		s.send(address, "That task is queued. Use /clearqueue to remove queued tasks.")
		return
	}
	s.send(address, fmt.Sprintf("Cancelling %s in %s...\nJob: %s", job.AgentName, job.ProjectID, jobReference(job.ID)))
}

func (s *Service) selectProject(address transport.Address, id string) {
	selected, ok := s.getProject(id)
	if !ok {
		s.send(address, "Unknown project. Run /projects to list available projects.")
		return
	}
	if err := s.store.SetSelectedProject(address, selected.ID); err != nil {
		s.log.Error("persist selected project", "error", err)
		s.send(address, "Could not save the selected project.")
		return
	}
	agentName := s.selectedAgent(address)
	if conversation, exists := s.store.Conversation(address, selected.ID, agentName); exists && conversation.ThreadID != "" {
		s.send(address, "Selected "+selected.ID+". Existing "+agentName+" context will be resumed.")
	} else {
		s.send(address, "Selected "+selected.ID+". The next message starts a new "+agentName+" context.")
	}
}

func (s *Service) selectAgent(address transport.Address, name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if _, ok := s.runners[name]; !ok {
		s.send(address, "Unknown agent. Run /agents to list available agents.")
		return
	}
	if err := s.store.SetSelectedAgent(address, name); err != nil {
		s.log.Error("persist selected agent", "error_type", fmt.Sprintf("%T", err))
		s.send(address, "Could not save the selected agent.")
		return
	}
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		s.send(address, "Selected "+name+". Select a project with /projects.")
		return
	}
	if conversation, exists := s.store.Conversation(address, projectID, name); exists && conversation.ThreadID != "" {
		s.send(address, "Selected "+name+". Existing context for "+projectID+" will be resumed.")
	} else {
		s.send(address, "Selected "+name+". The next message starts a new context for "+projectID+".")
	}
}

func (s *Service) newConversation(address transport.Address) {
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		s.send(address, "Select a project first with /project <project-id>.")
		return
	}
	agentName := s.selectedAgent(address)
	key := session.Key{Address: address, ProjectID: projectID, AgentName: agentName}
	status := s.sessions.Status(key)
	if status.Working || status.Queued > 0 || s.hasPersistedJobs(address, projectID, agentName) {
		s.send(address, "Cancel the active task and wait for the queue to clear before starting a new context.")
		return
	}
	if conversation, exists := s.store.Conversation(address, projectID, agentName); exists && conversation.ThreadID != "" {
		if resetter, ok := s.runners[agentName].(agent.Resetter); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := resetter.Reset(ctx, conversation.ThreadID)
			cancel()
			if err != nil {
				s.log.Warn("reset old agent session failed", "agent", agentName, "project", projectID, "error_type", fmt.Sprintf("%T", err))
			}
		}
	}
	if err := s.store.DeleteConversation(address, projectID, agentName); err != nil {
		s.log.Error("delete conversation", "error", err)
		s.send(address, "Could not reset the "+agentName+" context.")
		return
	}
	s.send(address, "The next message will start a new "+agentName+" context for "+projectID+".")
}

func (s *Service) cancel(address transport.Address) {
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		s.send(address, "No project is selected.")
		return
	}
	agentName := s.selectedAgent(address)
	if !s.sessions.Cancel(session.Key{Address: address, ProjectID: projectID, AgentName: agentName}) {
		s.send(address, agentName+" is not currently working in "+projectID+".")
		return
	}
	if conversation, exists := s.store.Conversation(address, projectID, agentName); exists {
		conversation.State = store.StateStopped
		conversation.LastActivity = time.Now().UTC()
		conversation.LastError = "cancellation requested"
		if err := s.store.PutConversation(conversation); err != nil {
			s.log.Error("persist cancellation state", "error_type", fmt.Sprintf("%T", err))
		}
	}
	s.send(address, "Cancelling the current "+agentName+" task...")
}

func (s *Service) cancelAll(address transport.Address) {
	count := s.sessions.CancelAddress(address)
	if count == 0 {
		s.send(address, "No agent tasks are currently running.")
		return
	}
	s.send(address, fmt.Sprintf("Cancelling %d running agent task(s)...", count))
}

func (s *Service) clearQueue(address transport.Address) {
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		s.send(address, "No project is selected.")
		return
	}
	agentName := s.selectedAgent(address)
	key := session.Key{Address: address, ProjectID: projectID, AgentName: agentName}
	status := s.sessions.Status(key)
	excludeID := int64(-1)
	if status.Working {
		excludeID = status.CurrentID
	}
	candidates := make(map[int64]store.PendingJob)
	for _, job := range s.store.Jobs() {
		if job.Address() == address && job.ProjectID == projectID && job.AgentName == agentName &&
			job.State != store.JobWorking && job.ID != excludeID {
			candidates[job.ID] = job
		}
	}
	removedMemory := s.sessions.ClearQueue(key)
	for _, job := range removedMemory {
		s.markDispatched(job.ID, false)
	}
	removed, err := s.store.ClearJobs(address, projectID, agentName, excludeID)
	if err != nil {
		s.log.Error("clear persisted jobs", "error_type", fmt.Sprintf("%T", err))
		s.send(address, "Could not clear the persisted queue.")
		return
	}
	for _, id := range removed {
		s.markDispatched(id, false)
		if job, ok := candidates[id]; ok {
			s.editJobStatus(job, fmt.Sprintf("Task removed from the queue.\nJob: %s", jobReference(id)), nil)
		}
	}
	if !status.Working {
		if conversation, exists := s.store.Conversation(address, projectID, agentName); exists && conversation.State == store.StateQueued {
			conversation.State = store.StateIdle
			conversation.LastActivity = time.Now().UTC()
			_ = s.store.PutConversation(conversation)
		}
	}
	s.send(address, fmt.Sprintf("Cleared %d queued or interrupted %s job(s) from %s.", len(removed), agentName, projectID))
}

func (s *Service) retryJob(address transport.Address, jobID int64) {
	job, ok := s.store.Job(jobID)
	if !ok || job.Address() != address {
		s.send(address, "Unknown job ID.")
		return
	}
	if selected := s.store.SelectedProject(address); selected != job.ProjectID {
		s.send(address, "Select "+job.ProjectID+" before retrying this job.")
		return
	}
	if selected := s.selectedAgent(address); selected != job.AgentName {
		s.send(address, "Select "+job.AgentName+" with /agent before retrying this job.")
		return
	}
	job, err := s.store.RetryJob(jobID)
	if err != nil {
		s.send(address, safeError(err))
		return
	}
	s.editJobStatus(job, fmt.Sprintf("Queued for retry with %s in %s.\nJob: %s", job.AgentName, job.ProjectID, jobReference(job.ID)), clearQueueButtons())
	if conversation, exists := s.store.Conversation(address, job.ProjectID, job.AgentName); exists {
		conversation.State = store.StateQueued
		conversation.LastActivity = time.Now().UTC()
		conversation.LastError = ""
		if err := s.store.PutConversation(conversation); err != nil {
			s.send(address, "Job is pending, but its session state could not be updated.")
		}
	}
	queued, err := s.dispatchJob(job)
	if err != nil {
		s.send(address, "Job was marked pending and will run when queue space is available.")
		return
	}
	if queued {
		s.send(address, fmt.Sprintf("Job %s was queued.", jobReference(jobID)))
	} else {
		s.send(address, fmt.Sprintf("Retrying job %s with %s in %s...", jobReference(jobID), job.AgentName, job.ProjectID))
	}
}

func (s *Service) sendLast(address transport.Address) {
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		s.send(address, "No project is selected.")
		return
	}
	agentName := s.selectedAgent(address)
	conversation, exists := s.store.Conversation(address, projectID, agentName)
	if !exists || conversation.LastResponse == "" {
		s.send(address, "No completed "+agentName+" response is stored for "+projectID+".")
		return
	}
	s.send(address, conversation.LastResponse)
}

func (s *Service) projectsMessage(address transport.Address) string {
	selected := s.store.SelectedProject(address)
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

func (s *Service) sendProjects(address transport.Address) {
	message := s.projectsMessage(address)
	projects := s.listProjects()
	rows := make([][]transport.Button, 0, (len(projects)+1)/2)
	var row []transport.Button
	for _, item := range projects {
		data := "project:" + item.ID
		if len(data) > 64 {
			continue
		}
		row = append(row, transport.Button{Text: item.ID, Data: data})
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	s.sendKeyboard(address, message, rows)
}

func (s *Service) agentsMessage(address transport.Address) string {
	selected := s.selectedAgent(address)
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

func (s *Service) sendAgents(address transport.Address) {
	names := make([]string, 0, len(s.runners))
	for name := range s.runners {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([][]transport.Button, 0, (len(names)+1)/2)
	for index := 0; index < len(names); index += 2 {
		row := []transport.Button{{Text: names[index], Data: "agent:" + names[index]}}
		if index+1 < len(names) {
			row = append(row, transport.Button{Text: names[index+1], Data: "agent:" + names[index+1]})
		}
		rows = append(rows, row)
	}
	s.sendKeyboard(address, s.agentsMessage(address), rows)
}

func (s *Service) sessionsMessage(address transport.Address) string {
	conversations := s.store.Conversations(address)
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

func (s *Service) queueMessage(address transport.Address) string {
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		return "No project is selected."
	}
	agentName := s.selectedAgent(address)
	var jobs []store.PendingJob
	for _, job := range s.store.Jobs() {
		if job.Address() == address && job.ProjectID == projectID && job.AgentName == agentName {
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
		lines = append(lines, fmt.Sprintf("%s — %s — %s", jobReference(job.ID), job.State, preview))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) statusMessage(address transport.Address) string {
	agentName := s.selectedAgent(address)
	runtime := fmt.Sprintf(
		"Agent Relay: %s\nTransport: %s\nAgent: %s (%s)\nUptime: %s",
		s.version,
		s.transportStatus(address),
		agentName,
		s.agentVersions[agentName],
		formatUptime(time.Since(s.startedAt)),
	)
	pendingDeliveries := 0
	deliveryAttempts := 0
	for _, message := range s.store.Outbox() {
		if message.Address() == address {
			pendingDeliveries++
			deliveryAttempts += message.Attempts
		}
	}
	runtime += fmt.Sprintf("\nPending deliveries: %d\nDelivery attempts: %d", pendingDeliveries, deliveryAttempts)
	projectID := s.store.SelectedProject(address)
	if projectID == "" {
		return runtime + "\nProject: not selected"
	}
	selected, ok := s.getProject(projectID)
	if !ok {
		return runtime + "\nProject: " + projectID + "\nState: unavailable"
	}
	conversation, exists := s.store.Conversation(address, projectID, agentName)
	status := s.sessions.Status(session.Key{Address: address, ProjectID: projectID, AgentName: agentName})
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
		if job.Address() == address && job.ProjectID == projectID && job.AgentName == agentName {
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

func (s *Service) transportStatus(address transport.Address) string {
	reporter, ok := s.sender(address).(transport.HealthReporter)
	if !ok {
		return "available"
	}
	health := reporter.Health()
	if health.State == "" {
		health.State = "unknown"
	}
	if health.Detail == "" {
		return health.State
	}
	return health.State + " (" + health.Detail + ")"
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

func (s *Service) selectedAgent(address transport.Address) string {
	name := s.store.SelectedAgent(address)
	if _, ok := s.runners[name]; ok {
		return name
	}
	return s.cfg.DefaultAgent
}
