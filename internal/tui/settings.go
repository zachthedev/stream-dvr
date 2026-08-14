package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Settings is what the model needs to read and write the config file.
//
// The model does no file I/O of its own. Path is here because the pane
// names the file it is about to rewrite: an operator running with an
// unexpected --config has to see that before they save over it.
type Settings interface {
	Load() (config.Config, error)
	Save(config.Config) error
	Path() string
}

// kind is how one setting is edited.
type kind int

// field describes one editable setting.
//
// The path is the dotted key, and it is the same string config.Docs is keyed
// by and config.Problem.Field carries. That is what lets help text and
// validation problems find their field without a mapping between them.
type field struct {
	path    string
	kind    kind
	choices []string
	get     func(config.Config) string
	set     func(*config.Config, string) error
}

// settingsMsg carries a config read from disk.
type settingsMsg struct {
	config config.Config
	err    error
}

// savedMsg carries the result of a save.
type savedMsg struct {
	err error
}

// ///////////////////////////////////////////////
// Kinds
// ///////////////////////////////////////////////

// settingsLabelCols is the width a field path is padded to, and
// settingsGutter is the cursor and the space that follow it. One
// definition, so the row and the editor drawn into it agree about how many
// columns are left.
const (
	settingsLabelCols = 34
	settingsGutter    = 3
	// minEditorWidth keeps a very narrow terminal from handing the editor
	// a width it cannot draw a cursor in.
	minEditorWidth = 8
)

const (
	// kindText opens a line editor.
	kindText kind = iota
	// kindBool flips on enter. A yes/no needs no editor.
	kindBool
	// kindChoice steps to the next accepted value on enter, so the
	// accepted set is discovered by pressing the key rather than by
	// reading the documentation.
	kindChoice
	// kindReadOnly is shown and cannot be changed here.
	kindReadOnly
)

// ///////////////////////////////////////////////
// The field table
// ///////////////////////////////////////////////

// configFields describes every setting the pane can show, in the order the
// config file holds them.
//
// The order is the ladder's order for the space table, cheapest first, which
// is the one thing about that table an operator has to understand.
func configFields() []field {
	fields := []field{{
		path: "library.root",
		kind: kindReadOnly,
		get:  func(c config.Config) string { return c.Library.Root },
	}}

	fields = append(fields, spaceFields()...)
	fields = append(fields, captureFields()...)
	fields = append(fields, namingFields()...)
	fields = append(fields, notifyFields()...)
	fields = append(fields, backfillFields()...)
	return append(fields, twitchFields()...)
}

// spaceFields describes the budget and the two rungs that spend it.
func spaceFields() []field {
	return []field{
		sizeField("space.max_size",
			func(c *config.Config) *config.Size { return &c.Space.MaxSize }),
		sizeField("space.min_free",
			func(c *config.Config) *config.Size { return &c.Space.MinFree }),

		boolField("space.recompress.enabled",
			func(c *config.Config) *bool { return &c.Space.Recompress.Enabled }),
		durationField("space.recompress.after",
			func(c *config.Config) *config.Duration { return &c.Space.Recompress.After }),
		choiceField("space.recompress.codec", config.RecompressCodecs,
			func(c *config.Config) *string { return &c.Space.Recompress.Codec }),
		intField("space.recompress.quality",
			func(c *config.Config) *int { return &c.Space.Recompress.Quality }),
		boolField("space.recompress.prefer_hardware",
			func(c *config.Config) *bool { return &c.Space.Recompress.PreferHardware }),
		intField("space.recompress.max_concurrent",
			func(c *config.Config) *int { return &c.Space.Recompress.MaxConcurrent }),
		boolField("space.recompress.keep_original",
			func(c *config.Config) *bool { return &c.Space.Recompress.KeepOriginal }),

		floatField("space.purge.watched_weight",
			func(c *config.Config) *float64 { return &c.Space.Purge.WatchedWeight }),
		floatField("space.purge.age_weight",
			func(c *config.Config) *float64 { return &c.Space.Purge.AgeWeight }),
		floatField("space.purge.refetchable_weight",
			func(c *config.Config) *float64 { return &c.Space.Purge.RefetchableWeight }),
		durationField("space.purge.protect_for",
			func(c *config.Config) *config.Duration { return &c.Space.Purge.ProtectFor }),
		durationField("space.purge.trash_grace",
			func(c *config.Config) *config.Duration { return &c.Space.Purge.TrashGrace }),
	}
}

// captureFields describes how broadcasts are recorded.
func captureFields() []field {
	return []field{
		durationField("capture.poll_interval",
			func(c *config.Config) *config.Duration { return &c.Capture.PollInterval }),
		listField("capture.quality",
			func(c *config.Config) *[]string { return &c.Capture.Quality }),
		durationField("capture.min_duration",
			func(c *config.Config) *config.Duration { return &c.Capture.MinDuration }),
		intField("capture.max_concurrent",
			func(c *config.Config) *int { return &c.Capture.MaxConcurrent }),
		choiceField("capture.container", config.Containers,
			func(c *config.Config) *string { return &c.Capture.Container }),
	}
}

// namingFields describes how a finished recording is named.
func namingFields() []field {
	return []field{
		textField("naming.template",
			func(c *config.Config) *string { return &c.Naming.Template }),
		textField("naming.timezone",
			func(c *config.Config) *string { return &c.Naming.Timezone }),
	}
}

// notifyFields describes where alerts go.
func notifyFields() []field {
	return []field{
		boolField("notify.desktop",
			func(c *config.Config) *bool { return &c.Notify.Desktop }),
		textField("notify.webhook_url",
			func(c *config.Config) *string { return &c.Notify.WebhookURL }),
		boolField("notify.on_recording_start",
			func(c *config.Config) *bool { return &c.Notify.OnRecordingStart }),
		boolField("notify.on_failure",
			func(c *config.Config) *bool { return &c.Notify.OnFailure }),
		boolField("notify.on_library_full",
			func(c *config.Config) *bool { return &c.Notify.OnLibraryFull }),
	}
}

// backfillFields describes how a recovery pass behaves, whether the
// recorder started it or the operator did.
func backfillFields() []field {
	return []field{
		boolField("backfill.automatic",
			func(c *config.Config) *bool { return &c.Backfill.Automatic }),
		durationField("backfill.settle",
			func(c *config.Config) *config.Duration { return &c.Backfill.Settle }),
		intField("backfill.max_concurrent",
			func(c *config.Config) *int { return &c.Backfill.MaxConcurrent }),
		intField("backfill.max_attempts",
			func(c *config.Config) *int { return &c.Backfill.MaxAttempts }),
		textField("backfill.rate_limit",
			func(c *config.Config) *string { return &c.Backfill.RateLimit }),
	}
}

// twitchFields describes the Twitch application this install acts as.
// Every install registers its own, so this is the one setting an operator
// has to fetch from outside before the metadata API works at all.
func twitchFields() []field {
	return []field{
		textField("twitch.client_id",
			func(c *config.Config) *string { return &c.Twitch.ClientID }),
	}
}

// textField edits a string as itself.
func textField(path string, at func(*config.Config) *string) field {
	return field{
		path: path,
		kind: kindText,
		get:  func(c config.Config) string { return *at(&c) },
		set: func(c *config.Config, value string) error {
			*at(c) = value
			return nil
		},
	}
}

// choiceField steps through an accepted set.
func choiceField(path string, choices []string, at func(*config.Config) *string) field {
	return field{
		path:    path,
		kind:    kindChoice,
		choices: choices,
		get:     func(c config.Config) string { return *at(&c) },
		set: func(c *config.Config, value string) error {
			if !slices.Contains(choices, value) {
				return fmt.Errorf("must be one of %s", strings.Join(choices, ", "))
			}
			*at(c) = value
			return nil
		},
	}
}

// boolField flips a yes or no.
func boolField(path string, at func(*config.Config) *bool) field {
	return field{
		path: path,
		kind: kindBool,
		get:  func(c config.Config) string { return strconv.FormatBool(*at(&c)) },
		set: func(c *config.Config, value string) error {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("must be true or false")
			}
			*at(c) = parsed
			return nil
		},
	}
}

// sizeField edits a byte count in the units the config file accepts.
func sizeField(path string, at func(*config.Config) *config.Size) field {
	return field{
		path: path,
		kind: kindText,
		get:  func(c config.Config) string { return at(&c).String() },
		set: func(c *config.Config, value string) error {
			parsed, err := config.ParseSize(value)
			if err != nil {
				return err
			}
			*at(c) = parsed
			return nil
		},
	}
}

// durationField edits a duration, accepting the d and w units the config
// file adds to Go's own.
func durationField(path string, at func(*config.Config) *config.Duration) field {
	return field{
		path: path,
		kind: kindText,
		get:  func(c config.Config) string { return at(&c).String() },
		set: func(c *config.Config, value string) error {
			parsed, err := config.ParseDuration(value)
			if err != nil {
				return err
			}
			*at(c) = parsed
			return nil
		},
	}
}

// intField edits a whole number.
func intField(path string, at func(*config.Config) *int) field {
	return field{
		path: path,
		kind: kindText,
		get:  func(c config.Config) string { return strconv.Itoa(*at(&c)) },
		set: func(c *config.Config, value string) error {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("must be a whole number")
			}
			*at(c) = parsed
			return nil
		},
	}
}

// floatField edits a weight.
func floatField(path string, at func(*config.Config) *float64) field {
	return field{
		path: path,
		kind: kindText,
		get:  func(c config.Config) string { return strconv.FormatFloat(*at(&c), 'g', -1, 64) },
		set: func(c *config.Config, value string) error {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return fmt.Errorf("must be a number")
			}
			*at(c) = parsed
			return nil
		},
	}
}

// listField edits a space-separated list, which is how the quality ladder
// reads in a single line.
func listField(path string, at func(*config.Config) *[]string) field {
	return field{
		path: path,
		kind: kindText,
		get:  func(c config.Config) string { return strings.Join(*at(&c), " ") },
		set: func(c *config.Config, value string) error {
			*at(c) = strings.Fields(value)
			return nil
		},
	}
}

// ///////////////////////////////////////////////
// Opening and keys
// ///////////////////////////////////////////////

// openSettings focuses the settings pane and reads the config from disk.
//
// It reads rather than reusing whatever the calendar started with, because
// the file can have been edited by hand since. Saving a stale copy would
// silently revert that edit.
func (m *Model) openSettings() tea.Cmd {
	if m.settings == nil {
		m.err = errors.New("no config file is reachable from here")
		return nil
	}

	m.pane = paneSettings
	m.field = 0
	m.editing = false
	m.saved, m.saveErr = false, nil
	m.fields = configFields()
	return m.loadSettings()
}

// handleSettingsKey applies a key press with the settings pane focused.
func (m *Model) handleSettingsKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.pane = paneCalendar
		return nil

	case "up", "k":
		m.field = max(m.field-1, 0)
	case "down", "j":
		m.field = min(m.field+1, max(m.rows()-1, 0))

	case "enter":
		return m.changeSelection()

	case "a":
		m.addChannel()

	case "ctrl+s":
		return m.saveSettings()

	case "r":
		return m.loadSettings()

	default:
		m.handleChannelKey(key)
	}
	return nil
}

// handleChannelKey applies the keys that only mean something on a channel
// row. Elsewhere in the pane they do nothing, and the footer says so by
// listing a different set.
func (m *Model) handleChannelKey(key string) {
	at, ok := m.selectedChannel()
	if !ok {
		return
	}

	switch key {
	case "b":
		m.editChannel(at, func(c *config.Channel) { c.Backfill = !c.Backfill })
	case "P":
		m.editChannel(at, func(c *config.Channel) {
			c.Platform = nextChoice(config.SupportedPlatforms, c.Platform)
		})
	case "n":
		m.editNameOfChannel(at)
	case "d":
		m.deleteChannel(at)
	}
}

// rows is how many lines the cursor can reach: every setting, then every
// watched channel.
func (m *Model) rows() int {
	return len(m.fields) + len(m.editor.Channels)
}

// handleEditKey feeds a key to the line editor.
//
// The editor owns every key while it is open, including q. Typing a q into a
// naming template must not close the application, so quitting is answered
// here rather than before the pane is consulted.
func (m *Model) handleEditKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		m.quit = true
		return tea.Quit

	case "enter":
		m.commitEdit()
		return nil

	case "esc":
		m.editing = false
		return nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// changeSelection acts on whatever the cursor is on.
func (m *Model) changeSelection() tea.Cmd {
	if at, ok := m.selectedChannel(); ok {
		m.editChannel(at, func(c *config.Channel) { c.Enabled = !c.Enabled })
		return nil
	}
	return m.changeField()
}

// changeField acts on the field under the cursor, by its kind.
func (m *Model) changeField() tea.Cmd {
	target, ok := m.selectedField()
	if !ok {
		return nil
	}

	switch target.kind {
	case kindReadOnly:
		m.saveErr = fmt.Errorf(
			"%s is not editable here; use 'stream-dvr library init' or 'library adopt'", target.path)

	case kindBool:
		m.record(m.applyEdit(target, strconv.FormatBool(target.get(m.editor) != "true")))

	case kindChoice:
		m.record(m.applyEdit(target, nextChoice(target.choices, target.get(m.editor))))

	case kindText:
		m.beginEdit(target.get(m.editor), func(value string) error {
			return m.applyEdit(target, value)
		})
	}
	return nil
}

// beginEdit opens the line editor over a value, with what to do when it is
// accepted.
//
// The commit is carried rather than looked up again, so an editor over a
// channel name and one over a setting are the same editor.
func (m *Model) beginEdit(value string, commit func(string) error) {
	input := textinput.New()
	input.Prompt = ""
	input.SetValue(value)
	input.CursorEnd()
	input.Focus()

	// A blinking cursor wakes the program twice a second for as long as the
	// editor is open. This is a tool that sits open on a desktop, and the
	// static block says where the cursor is just as well.
	//
	// The set is taken for the scheme the rest of the screen resolved
	// against, so the editor's greys match the greys around it.
	editorStyles := textinput.DefaultStyles(m.styles.dark)
	editorStyles.Cursor.Blink = false
	input.SetStyles(editorStyles)

	// Given the width it is drawn into, so a value longer than the row
	// scrolls inside its own viewport. Without one the input reports its
	// full length, the panel elides the row, and the operator types past
	// the visible edge with the cursor off screen.
	if width := m.editorWidth(); width > 0 {
		input.SetWidth(width)
	}

	m.input = input
	m.commit = commit
	m.editing = true
}

// editorWidth is how many columns the line editor has to draw in.
//
// The panel's inner width less the label column and the gutter the row
// already spends, floored so a very narrow terminal still leaves the
// editor something rather than a negative width.
func (m *Model) editorWidth() int {
	inner := m.frame.Calendar.inner()
	return max(inner.W-settingsLabelCols-settingsGutter, minEditorWidth)
}

// commitEdit applies what was typed and closes the editor.
//
// A value the field cannot parse leaves the editor open with the text still
// in it, so a mistyped duration is corrected rather than retyped.
func (m *Model) commitEdit() {
	if m.commit == nil {
		m.editing = false
		return
	}

	if err := m.commit(m.input.Value()); err != nil {
		m.saveErr = err
		return
	}
	m.editing = false
}

// record surfaces a rejected edit and leaves an accepted one silent.
func (m *Model) record(err error) {
	if err != nil {
		m.saveErr = err
	}
}

// applyEdit writes one value into the working config and revalidates.
//
// A parse failure is separate from a validation problem: the first means
// the text is not a value of that kind, and the second means it is a value
// the config refuses.
func (m *Model) applyEdit(target field, value string) error {
	edited := m.editor
	if err := target.set(&edited, value); err != nil {
		return fmt.Errorf("%s %w", target.path, err)
	}

	m.editor = edited
	m.settle()
	return nil
}

// settle normalizes the working config and revalidates it, which is what
// every edit ends with however it reached the config.
func (m *Model) settle() {
	m.editor.Normalize()
	m.saved, m.saveErr = false, nil
	m.dirty = true
	m.problems = validationProblems(m.editor)
}

// nextChoice steps to the value after current, wrapping.
func nextChoice(choices []string, current string) string {
	if len(choices) == 0 {
		return current
	}

	at := slices.Index(choices, current)
	return choices[(at+1)%len(choices)]
}

// selectedField returns the field under the cursor.
func (m *Model) selectedField() (field, bool) {
	if m.field < 0 || m.field >= len(m.fields) {
		return field{}, false
	}
	return m.fields[m.field], true
}

// selectedChannel returns the index of the channel under the cursor.
//
// The channels continue the same cursor rather than owning a second one.
// The pane is one list to scroll through, and a separate focus to switch
// into is a mode an operator has to remember they are in.
func (m *Model) selectedChannel() (int, bool) {
	at := m.field - len(m.fields)
	if at < 0 || at >= len(m.editor.Channels) {
		return 0, false
	}
	return at, true
}

// ///////////////////////////////////////////////
// Channels
// ///////////////////////////////////////////////

// editChannel applies a change to the channel under the cursor.
//
// The index is checked rather than trusted. An edit closes over the
// channel it was opened on, and the config underneath can be replaced
// while the editor is open, so by the time this runs the list may be
// shorter than the position the caller is holding.
func (m *Model) editChannel(at int, apply func(*config.Channel)) {
	if at < 0 || at >= len(m.editor.Channels) {
		return
	}

	channels := slices.Clone(m.editor.Channels)
	apply(&channels[at])

	m.editor.Channels = channels
	m.settle()
}

// addChannel appends a watched channel and opens its name for editing.
//
// It starts disabled and unnamed, which is a validation problem, so the
// save refuses until the operator finishes the row. That is the same guard
// every other setting gets rather than a special case for this one.
func (m *Model) addChannel() {
	m.editor.Channels = append(slices.Clone(m.editor.Channels),
		config.Channel{Platform: config.SupportedPlatforms[0]})
	m.field = len(m.fields) + len(m.editor.Channels) - 1
	m.settle()

	m.editNameOfChannel(len(m.editor.Channels) - 1)
}

// deleteChannel removes the channel under the cursor.
//
// It takes no confirmation. Nothing recorded is lost: the recordings stay
// in the library and the row can be added back, so the cost is retyping a
// name rather than a broadcast.
func (m *Model) deleteChannel(at int) {
	m.editor.Channels = slices.Delete(slices.Clone(m.editor.Channels), at, at+1)
	m.field = min(m.field, len(m.fields)+max(len(m.editor.Channels)-1, 0))
	m.settle()
}

// editNameOfChannel opens the line editor over a channel's name.
func (m *Model) editNameOfChannel(at int) {
	m.beginEdit(m.editor.Channels[at].Name, func(value string) error {
		m.editChannel(at, func(c *config.Channel) { c.Name = value })
		return nil
	})
}

// ///////////////////////////////////////////////
// Commands
// ///////////////////////////////////////////////

// loadSettings reads the config off the event loop.
func (m *Model) loadSettings() tea.Cmd {
	settings := m.settings

	return func() tea.Msg {
		cfg, err := settings.Load()
		return settingsMsg{config: cfg, err: err}
	}
}

// saveSettings writes the working config, refusing while any problem stands.
//
// Refusing rather than writing and letting the next Load complain is the
// point: a config the daemon cannot read stops the recorder at its next
// restart, and the operator would not find out until a broadcast was missed.
func (m *Model) saveSettings() tea.Cmd {
	if len(m.problems) > 0 {
		m.saveErr = errors.New("fix the problems below before saving")
		return nil
	}

	settings := m.settings
	saving := m.editor

	return func() tea.Msg {
		return savedMsg{err: settings.Save(saving)}
	}
}

// applySettings folds a config read into the model.
func (m *Model) applySettings(msg settingsMsg) tea.Cmd {
	if msg.err != nil {
		m.err = msg.err
		return nil
	}

	m.err = nil
	m.editor = msg.config
	m.problems = validationProblems(m.editor)

	// An open editor was opened over the config this just replaced, and it
	// holds a position in a list that no longer exists. Closing it drops
	// one unsaved value; leaving it open commits that value onto whichever
	// channel now sits at that index, or past the end of a shorter list.
	if m.editing {
		m.editing = false
		m.commit = nil
	}
	m.field = min(m.field, max(m.rows()-1, 0))
	return nil
}

// applySaved folds a completed save into the model, then reads the file
// back so the pane shows the normalized values rather than what was typed.
func (m *Model) applySaved(msg savedMsg) tea.Cmd {
	m.saveErr = msg.err
	if msg.err != nil {
		return nil
	}

	m.saved = true
	m.dirty = false
	return m.loadSettings()
}

// validationProblems returns every problem a config carries, and nothing
// when it is valid.
func validationProblems(cfg config.Config) []config.Problem {
	var invalid *config.ValidationError
	if err := cfg.Validate(); errors.As(err, &invalid) {
		return invalid.Problems
	}
	return nil
}

// ///////////////////////////////////////////////
// View
// ///////////////////////////////////////////////

// settingsView draws the fields, the help for the one under the cursor, and
// the state of the file.
func (m *Model) settingsView() (head, rows, tail []string, at int) {
	head = []string{m.styles.dim.Render(escape.Text(m.settings.Path())), ""}
	if m.err != nil {
		head = append(head, m.styles.err.Render(escape.Text(m.err.Error())), "")
	}

	problems := problemsByField(m.problems)
	for i, target := range m.fields {
		rows = append(rows, trimLine(m.fieldLine(i, target, problems[target.path])))
	}

	rows = append(rows, "", m.styles.heading.Render("channels"))
	if len(m.editor.Channels) == 0 {
		rows = append(rows, m.styles.dim.Render("  none watched, press a to add one"))
	}
	for index, channel := range m.editor.Channels {
		rows = append(rows, trimLine(m.channelLine(index, channel, channelProblem(m.problems, index))))
	}

	// The help and the save state stay pinned under the list. They describe
	// the row the cursor is on, and a description that scrolls away from
	// what it describes helps nobody.
	tail = append(tail, "")
	tail = append(tail, splitLines(m.fieldHelp())...)
	tail = append(tail, splitLines(m.settingsStatus())...)

	// The channel rows follow the field rows, and the two blank rows and the
	// heading between them push the cursor down by three.
	at = m.field
	if m.field >= len(m.fields) {
		at += 3
	}
	return head, rows, tail, at
}

// trimLine drops the newline a line renderer ends with, so a block can be
// laid out as rows rather than as one string.
func trimLine(text string) string {
	return strings.TrimRight(text, "\n")
}

// splitLines breaks a rendered block into rows.
func splitLines(text string) []string {
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// channelLine renders one watched channel.
func (m *Model) channelLine(at int, channel config.Channel, problem string) string {
	cursor := "  "
	if m.field == len(m.fields)+at {
		cursor = m.styles.heading.Render("> ")
	}

	box := "[ ]"
	if channel.Enabled {
		box = "[x]"
	}

	name := escape.Text(channel.Platform + "/" + channel.Name)
	if channel.Name == "" {
		name = escape.Text(channel.Platform) + "/" + m.styles.dim.Render("(press n to name it)")
	}
	if at == m.editorRow() {
		name = escape.Text(channel.Platform) + "/" + m.input.View()
	}

	line := cursor + box + " " + name
	if channel.Backfill {
		line += m.styles.dim.Render("   backfill")
	}
	line += "\n"

	if problem == "" {
		return line
	}
	return line + m.styles.err.Render("      "+problem) + "\n"
}

// onChannel reports whether the cursor is on a channel rather than a
// setting, which is what changes the keys the footer lists.
func (m *Model) onChannel() bool {
	_, ok := m.selectedChannel()
	return ok
}

// editorRow returns the channel index the line editor is open over, or -1.
func (m *Model) editorRow() int {
	if !m.editing {
		return -1
	}

	at, ok := m.selectedChannel()
	if !ok {
		return -1
	}
	return at
}

// channelProblem returns the problem naming a channel row.
//
// Validation names a row two ways: "channels[0]" for a fault in the row
// itself, such as a duplicate, and "channels[0].name" for one in a field of
// it. The closing bracket terminates the index, so neither form can match a
// row whose number merely starts with this one's.
func channelProblem(problems []config.Problem, at int) string {
	prefix := fmt.Sprintf("channels[%d]", at)

	for _, problem := range problems {
		switch {
		case problem.Field == prefix:
			return problem.Detail
		case strings.HasPrefix(problem.Field, prefix+"."):
			return strings.TrimPrefix(problem.Field, prefix+".") + " " + problem.Detail
		}
	}
	return ""
}

// fieldLine renders one setting, with the editor in place of its value when
// that field is being edited.
func (m *Model) fieldLine(i int, target field, problem string) string {
	cursor := "  "
	if i == m.field {
		cursor = m.styles.heading.Render("> ")
	}

	value := escape.Text(target.get(m.editor))
	if i == m.field && m.editing {
		value = m.input.View()
	}
	if target.kind == kindReadOnly {
		value = m.styles.dim.Render(value + "  (set with 'library init')")
	}

	line := fmt.Sprintf("%s%-34s %s\n", cursor, target.path, value)
	if problem == "" {
		return line
	}
	return line + m.styles.err.Render("  "+strings.Repeat(" ", 34)+problem) + "\n"
}

// fieldHelp shows the documentation for the field under the cursor.
//
// It comes from config.Docs rather than a second copy here, so the
// interface and the config file's comments can never disagree.
func (m *Model) fieldHelp() string {
	target, ok := m.selectedField()
	if !ok {
		return ""
	}

	doc, ok := config.Docs[target.path]
	if !ok || doc.Comment == "" {
		return ""
	}
	return m.styles.dim.Render(doc.Comment) + "\n"
}

// settingsStatus reports what the last save or edit did.
func (m *Model) settingsStatus() string {
	switch {
	case m.saveErr != nil:
		return "\n" + m.styles.err.Render(escape.Text(m.saveErr.Error())) + "\n"
	case m.saved:
		// True and not obvious: the daemon reads its config once, at
		// startup.
		return "\n" + m.styles.ok.Render(
			"saved; the running recorder keeps its current settings until it restarts") + "\n"
	case len(m.problems) > 0:
		return "\n" + m.styles.err.Render(fmt.Sprintf("%d %s to fix before this can be saved",
			len(m.problems), plural(len(m.problems), "problem", "problems"))) + "\n"
	default:
		return ""
	}
}

// problemsByField indexes validation problems by the field they name.
func problemsByField(problems []config.Problem) map[string]string {
	byField := make(map[string]string, len(problems))
	for _, problem := range problems {
		byField[problem.Field] = problem.Detail
	}
	return byField
}
