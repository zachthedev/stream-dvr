package tui

import (
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/config"
)

// ///////////////////////////////////////////////
// Fakes
// ///////////////////////////////////////////////

// fakeSettings serves a config from memory and records what was saved.
type fakeSettings struct {
	config config.Config

	saves   []config.Config
	loads   int
	loadErr error
	saveErr error
}

// exampleRoot is a library root this platform calls absolute.
//
// The config refuses a relative root, so a fixture written in one
// platform's spelling makes every case below fail on the others for a
// reason that has nothing to do with what they test: a drive letter is not
// an absolute path on Linux, and a leading slash is not one on Windows.
var exampleRoot = func() string {
	if runtime.GOOS == "windows" {
		return `D:\recordings`
	}
	return "/srv/recordings"
}()

func (f *fakeSettings) Load() (config.Config, error) {
	f.loads++
	if f.loadErr != nil {
		return config.Config{}, f.loadErr
	}
	return f.config, nil
}

func (f *fakeSettings) Save(cfg config.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}

	f.saves = append(f.saves, cfg)
	f.config = cfg
	return nil
}

func (f *fakeSettings) Path() string { return filepath.Join(exampleRoot, "config.toml") }

// ///////////////////////////////////////////////
// Harness
// ///////////////////////////////////////////////

// settingsFixture returns a valid config to edit.
func settingsFixture() *fakeSettings {
	cfg := config.DefaultConfig()
	cfg.Library.Root = exampleRoot
	cfg.Channels = []config.Channel{
		{Platform: config.PlatformTwitch, Name: "examplechannel", Enabled: true},
	}
	return &fakeSettings{config: cfg}
}

// openedSettings returns a model with the settings pane open.
func openedSettings(t *testing.T, settings *fakeSettings) *Model {
	t.Helper()

	model := newOptionsModel(t, Options{Library: library(), Settings: settings})
	press(t, model, "e")
	return model
}

// focusField moves the cursor onto a named setting.
func focusField(t *testing.T, model *Model, path string) {
	t.Helper()

	for i, target := range model.fields {
		if target.path == path {
			model.field = i
			return
		}
	}
	t.Fatalf("the settings pane has no field %q", path)
}

// typeInto sends each rune of text to the open line editor.
func typeInto(t *testing.T, model *Model, text string) {
	t.Helper()

	for _, r := range text {
		_, cmd := model.Update(keyPress(string(r)))
		drain(t, model, cmd)
	}
}

// clearInput empties the open line editor.
func clearInput(t *testing.T, model *Model) {
	t.Helper()

	for range len(model.input.Value()) {
		_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		drain(t, model, cmd)
	}
}

// ///////////////////////////////////////////////
// Opening
// ///////////////////////////////////////////////

func TestModel_SettingsOpensOnTheFileOnDisk(t *testing.T) {
	// The file can have been edited by hand since the calendar started.
	// Saving whatever the calendar loaded would revert that edit silently.
	settings := settingsFixture()
	model := openedSettings(t, settings)

	if model.pane != paneSettings {
		t.Errorf("pane = %d after e, want the settings pane", model.pane)
	}
	if settings.loads != 1 {
		t.Errorf("Load() called %d times, want 1", settings.loads)
	}

	// Fields near the top of the list. The list scrolls now, so a field
	// thirty rows down is off screen until the cursor reaches it, which is
	// the whole point of it scrolling.
	view := render(t, model)
	for _, want := range []string{"config.toml", "library.root", "space.max_size"} {
		if !strings.Contains(view, want) {
			t.Errorf("the settings pane does not show %q:\n%s", want, view)
		}
	}
}

func TestModel_SettingsWithoutAConfigStoreSaysSo(t *testing.T) {
	model := newModel(t, library(), nil)

	press(t, model, "e")

	if model.pane == paneSettings {
		t.Error("the settings pane opened with no config behind it")
	}
	if model.Err() == nil || !strings.Contains(model.Err().Error(), "no config file") {
		t.Errorf("Err() = %v, want it to say no config file is reachable", model.Err())
	}
}

func TestModel_SettingsReportsALoadFailure(t *testing.T) {
	settings := settingsFixture()
	settings.loadErr = errors.New("config.toml is not readable")
	model := openedSettings(t, settings)

	if !strings.Contains(render(t, model), "not readable") {
		t.Error("the settings pane does not say why it has no config")
	}
}

func TestModel_SettingsShowsTheDocumentationForTheFocusedField(t *testing.T) {
	// The help comes from config.Docs rather than a second copy, so the
	// interface and the config file's comments cannot disagree.
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "capture.container")

	view := render(t, model)
	doc := config.Docs["capture.container"].Comment
	if doc == "" {
		t.Fatal("capture.container has no documented comment to show")
	}
	if !strings.Contains(view, strings.Split(doc, "\n")[0]) {
		t.Errorf("the pane does not show the field's documentation:\n%s", view)
	}
}

// ///////////////////////////////////////////////
// Editing
// ///////////////////////////////////////////////

func TestModel_SettingsTogglesABool(t *testing.T) {
	// A yes or no needs no line editor, so enter flips it.
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "space.recompress.enabled")

	pressNamed(t, model, tea.KeyEnter)

	if !model.editor.Space.Recompress.Enabled {
		t.Error("enter did not turn recompress on")
	}
	if model.editing {
		t.Error("a bool opened the line editor")
	}

	pressNamed(t, model, tea.KeyEnter)
	if model.editor.Space.Recompress.Enabled {
		t.Error("enter did not turn recompress back off")
	}
}

func TestModel_SettingsStepsThroughAChoice(t *testing.T) {
	// Pressing the key is how the accepted set is discovered, so it wraps
	// rather than stopping at the last value.
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "capture.container")

	seen := make([]string, 0, len(config.Containers)+1)
	for range len(config.Containers) + 1 {
		pressNamed(t, model, tea.KeyEnter)
		seen = append(seen, model.editor.Capture.Container)
	}

	for _, container := range config.Containers {
		if !strings.Contains(strings.Join(seen, " "), container) {
			t.Errorf("stepping never reached %q, saw %v", container, seen)
		}
	}
	if seen[len(seen)-1] != seen[0] {
		t.Errorf("stepping did not wrap: ended on %q, started on %q", seen[len(seen)-1], seen[0])
	}
}

func TestModel_SettingsEditsAText(t *testing.T) {
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "space.max_size")

	pressNamed(t, model, tea.KeyEnter)
	if !model.editing {
		t.Fatal("enter did not open the line editor over a text field")
	}

	clearInput(t, model)
	typeInto(t, model, "750GB")
	pressNamed(t, model, tea.KeyEnter)

	if model.editing {
		t.Error("the line editor stayed open after a value it accepted")
	}
	if got := model.editor.Space.MaxSize; got != 750*config.Gigabyte {
		t.Errorf("MaxSize = %s, want 750GB", got)
	}
}

func TestModel_SettingsKeepsTheEditorOpenOnAValueItCannotParse(t *testing.T) {
	// A mistyped duration is corrected rather than retyped.
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "space.purge.protect_for")
	before := model.editor.Space.Purge.ProtectFor

	pressNamed(t, model, tea.KeyEnter)
	clearInput(t, model)
	typeInto(t, model, "eleven fortnights")
	pressNamed(t, model, tea.KeyEnter)

	if !model.editing {
		t.Error("the line editor closed over a value it could not parse")
	}
	if model.editor.Space.Purge.ProtectFor != before {
		t.Error("a value that would not parse still reached the config")
	}
	if !strings.Contains(render(t, model), "space.purge.protect_for") {
		t.Error("the pane does not name the field that would not parse")
	}
}

func TestModel_SettingsEditIsCancellable(t *testing.T) {
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "naming.timezone")
	before := model.editor.Naming.Timezone

	pressNamed(t, model, tea.KeyEnter)
	clearInput(t, model)
	typeInto(t, model, "Antarctica/Troll")
	pressNamed(t, model, tea.KeyEsc)

	if model.editing {
		t.Fatal("esc left the line editor open")
	}
	if model.editor.Naming.Timezone != before {
		t.Errorf("Timezone = %q after esc, want %q", model.editor.Naming.Timezone, before)
	}
}

func TestModel_SettingsEditorTakesQAsALetter(t *testing.T) {
	// The naming template is full of ordinary letters. A q typed into it
	// must not close the application.
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "naming.template")

	pressNamed(t, model, tea.KeyEnter)
	clearInput(t, model)
	typeInto(t, model, "{author}/q/{title}.{ext}")

	if model.Quit() {
		t.Fatal("typing a q into the naming template quit the application")
	}

	pressNamed(t, model, tea.KeyEnter)
	if got := model.editor.Naming.Template; got != "{author}/q/{title}.{ext}" {
		t.Errorf("Template = %q, want the typed value", got)
	}
}

func TestModel_SettingsRefusesToEditTheLibraryRoot(t *testing.T) {
	// The open library and store handles are derived from the root, so
	// changing it mid-session would leave the calendar reading a library it
	// no longer names.
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "library.root")

	pressNamed(t, model, tea.KeyEnter)

	if model.editing {
		t.Fatal("the line editor opened over the library root")
	}
	view := render(t, model)
	if !strings.Contains(view, "not editable here") {
		t.Errorf("the pane does not say why the root cannot be changed:\n%s", view)
	}
	if !strings.Contains(view, "library init") {
		t.Errorf("the pane does not say what does change the root:\n%s", view)
	}
}

// ///////////////////////////////////////////////
// Saving
// ///////////////////////////////////////////////

func TestModel_SettingsSavesThroughTheStore(t *testing.T) {
	settings := settingsFixture()
	model := openedSettings(t, settings)
	focusField(t, model, "capture.max_concurrent")
	pressNamed(t, model, tea.KeyEnter)
	clearInput(t, model)
	typeInto(t, model, "5")
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "ctrl+s")

	if len(settings.saves) != 1 {
		t.Fatalf("Save() called %d times, want 1", len(settings.saves))
	}
	if got := settings.saves[0].Capture.MaxConcurrent; got != 5 {
		t.Errorf("the saved config has MaxConcurrent = %d, want 5", got)
	}
	if settings.loads != 2 {
		t.Errorf("Load() called %d times, want a reload after the save", settings.loads)
	}

	view := render(t, model)
	if !strings.Contains(view, "saved") {
		t.Errorf("the pane does not confirm the save:\n%s", view)
	}
	if !strings.Contains(view, "until it restarts") {
		t.Errorf("the pane does not say the running recorder keeps its settings:\n%s", view)
	}
}

func TestModel_SettingsRefusesToSaveAConfigThatWouldNotLoad(t *testing.T) {
	// A config the daemon cannot read stops the recorder at its next
	// restart, and the operator would not find out until a broadcast was
	// missed.
	settings := settingsFixture()
	model := openedSettings(t, settings)
	focusField(t, model, "capture.max_concurrent")
	pressNamed(t, model, tea.KeyEnter)
	clearInput(t, model)
	typeInto(t, model, "0")
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "ctrl+s")

	if len(settings.saves) != 0 {
		t.Fatalf("Save() ran over %d problems", len(model.problems))
	}
	view := render(t, model)
	if !strings.Contains(view, "must be at least 1") {
		t.Errorf("the pane does not show the problem against its field:\n%s", view)
	}
	if !strings.Contains(view, "before saving") {
		t.Errorf("the pane does not say the save was refused:\n%s", view)
	}
}

func TestModel_SettingsReportsASaveFailure(t *testing.T) {
	settings := settingsFixture()
	settings.saveErr = errors.New("config.toml is read-only")
	model := openedSettings(t, settings)

	press(t, model, "ctrl+s")

	view := render(t, model)
	if !strings.Contains(view, "read-only") {
		t.Errorf("the pane does not say why the save failed:\n%s", view)
	}
	if strings.Contains(view, "saved;") {
		t.Errorf("the pane reports a save that failed as done:\n%s", view)
	}
}

// ///////////////////////////////////////////////
// Channels
// ///////////////////////////////////////////////

// focusChannel moves the cursor onto a channel row.
func focusChannel(t *testing.T, model *Model, at int) {
	t.Helper()

	if at >= len(model.editor.Channels) {
		t.Fatalf("the config holds %d channels, want one at %d", len(model.editor.Channels), at)
	}
	model.field = len(model.fields) + at
}

func TestModel_SettingsListsTheWatchedChannels(t *testing.T) {
	model := openedSettings(t, settingsFixture())

	view := render(t, model)
	if !strings.Contains(view, "twitch/examplechannel") {
		t.Errorf("the pane does not list the watched channel:\n%s", view)
	}
}

func TestModel_SettingsTogglesAChannel(t *testing.T) {
	// This is the config change an operator makes most often: stop watching
	// a channel without losing the row that names it.
	settings := settingsFixture()
	model := openedSettings(t, settings)
	focusChannel(t, model, 0)

	pressNamed(t, model, tea.KeyEnter)
	if model.editor.Channels[0].Enabled {
		t.Error("enter did not stop watching the channel")
	}

	press(t, model, "b")
	if !model.editor.Channels[0].Backfill {
		t.Error("b did not turn backfill on")
	}

	press(t, model, "P")
	if model.editor.Channels[0].Platform == config.PlatformTwitch {
		t.Error("P did not step the platform")
	}
}

func TestModel_SettingsAddsAChannelAndRefusesToSaveItUnnamed(t *testing.T) {
	// A new row starts unnamed, which is a validation problem, so the save
	// refuses until the operator finishes it. That is the same guard every
	// other setting gets rather than a special case for this one.
	settings := settingsFixture()
	model := openedSettings(t, settings)

	press(t, model, "a")

	if len(model.editor.Channels) != 2 {
		t.Fatalf("the config holds %d channels, want 2", len(model.editor.Channels))
	}
	if !model.editing {
		t.Error("adding a channel did not open its name for editing")
	}
	if _, ok := model.selectedChannel(); !ok {
		t.Error("the cursor did not move onto the new channel")
	}

	pressNamed(t, model, tea.KeyEsc)
	press(t, model, "ctrl+s")
	if len(settings.saves) != 0 {
		t.Error("an unnamed channel was saved")
	}
	if !strings.Contains(render(t, model), "name") {
		t.Errorf("the pane does not say the channel needs a name:\n%s", render(t, model))
	}
}

func TestModel_SettingsNamesAChannel(t *testing.T) {
	settings := settingsFixture()
	model := openedSettings(t, settings)
	press(t, model, "a")

	typeInto(t, model, "anotherchannel")
	pressNamed(t, model, tea.KeyEnter)

	if got := model.editor.Channels[1].Name; got != "anotherchannel" {
		t.Errorf("the new channel is named %q, want the typed name", got)
	}

	press(t, model, "ctrl+s")
	if len(settings.saves) != 1 {
		t.Fatalf("Save() called %d times once the channel was named, want 1", len(settings.saves))
	}
	if got := settings.saves[0].Channels[1].Name; got != "anotherchannel" {
		t.Errorf("the saved config names the channel %q", got)
	}
}

func TestModel_SettingsDeletesAChannel(t *testing.T) {
	settings := settingsFixture()
	model := openedSettings(t, settings)
	focusChannel(t, model, 0)

	press(t, model, "d")

	if len(model.editor.Channels) != 0 {
		t.Fatalf("the config holds %d channels, want none", len(model.editor.Channels))
	}
	if _, ok := model.selectedChannel(); ok {
		t.Error("the cursor still points at a channel that is gone")
	}

	press(t, model, "ctrl+s")
	if len(settings.saves) != 1 || len(settings.saves[0].Channels) != 0 {
		t.Error("the deletion did not reach the saved config")
	}
}

func TestModel_SettingsChannelKeysDoNothingOnASetting(t *testing.T) {
	// The footer lists a different set on a setting, so these keys have to
	// be inert there rather than acting on whichever channel is nearest.
	model := openedSettings(t, settingsFixture())
	focusField(t, model, "capture.container")
	before := model.editor.Channels[0]

	for _, key := range []string{"b", "P", "n", "d"} {
		press(t, model, key)
	}

	if len(model.editor.Channels) != 1 {
		t.Fatalf("the channel list changed from a settings row: %d channels", len(model.editor.Channels))
	}
	if !reflect.DeepEqual(model.editor.Channels[0], before) {
		t.Errorf("Channels[0] = %+v, want %+v", model.editor.Channels[0], before)
	}
	if model.editing {
		t.Error("a channel key opened an editor over a setting")
	}
}

func TestChannelProblem_MatchesTheRowExactly(t *testing.T) {
	// Validation names a row two ways, and the row's own fault and a fault
	// in one of its fields read differently. Row 10 is here because a
	// numbering without the closing bracket would let row 1 claim it.
	problems := []config.Problem{
		{Field: "channels[10].name", Detail: "must not be empty"},
		{Field: "channels[2]", Detail: "duplicates channels[0]"},
	}

	cases := []struct {
		name string
		at   int
		want string
	}{
		{name: "a row with no problem", at: 1, want: ""},
		{name: "a problem on a field of the row", at: 10, want: "name must not be empty"},
		{name: "a problem on the row itself", at: 2, want: "duplicates channels[0]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelProblem(problems, tc.at); got != tc.want {
				t.Errorf("channelProblem() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// The field table
// ///////////////////////////////////////////////

func TestConfigFields_CoversEverySettingByItsDottedPath(t *testing.T) {
	// The path is what joins a field to its documentation and to any
	// validation problem naming it. A field whose path is not a real config
	// key silently loses both.
	for _, target := range configFields() {
		t.Run(target.path, func(t *testing.T) {
			if _, ok := config.Docs[target.path]; !ok {
				t.Errorf("%q is not a documented config key", target.path)
			}
		})
	}
}

func TestConfigFields_ShowsEverySettingOrSaysWhyNot(t *testing.T) {
	// The other direction. A documented setting the daemon reads and nothing
	// here offers is one an operator can only reach by editing the file, and
	// the pane gives no sign it exists.
	shown := map[string]bool{}
	for _, target := range configFields() {
		shown[target.path] = true
	}

	// A key other keys nest under is a table header in the file rather than
	// a setting anyone edits, so it is derived rather than listed.
	section := func(key string) bool {
		for other := range config.Docs {
			if strings.HasPrefix(other, key+".") {
				return true
			}
		}
		return false
	}

	elsewhere := map[string]string{
		"schema_version":    "the format version, which migration owns and an operator never chooses",
		"channels.platform": "the channel editor owns this row",
		"channels.name":     "the channel editor owns this row",
		"channels.enabled":  "the channel editor owns this row",
		"channels.quality":  "the channel editor owns this row",
		"channels.backfill": "the channel editor owns this row",
	}

	for key := range config.Docs {
		if shown[key] || section(key) {
			continue
		}
		if elsewhere[key] == "" {
			t.Errorf("%q is documented, validated and read by the daemon, and the settings pane neither shows it nor says why not", key)
		}
	}

	for key := range elsewhere {
		if _, documented := config.Docs[key]; !documented {
			t.Errorf("%q is excused here and is not a documented config key", key)
		}
		if shown[key] {
			t.Errorf("%q is excused here and the pane shows it, so the excuse is stale", key)
		}
	}
}

func TestConfigFields_RoundTripsEveryValue(t *testing.T) {
	// Every field renders its value as text and reads that same text back.
	// A getter and setter that disagree turn opening a field and pressing
	// enter into a silent edit.
	cfg := config.DefaultConfig()

	for _, target := range configFields() {
		t.Run(target.path, func(t *testing.T) {
			if target.kind == kindReadOnly {
				t.Skip("read-only fields have no setter")
			}

			edited := cfg
			if err := target.set(&edited, target.get(cfg)); err != nil {
				t.Fatalf("set(get()) err = %v, want nil", err)
			}
			if got := target.get(edited); got != target.get(cfg) {
				t.Errorf("the value changed by being written back: %q, want %q", got, target.get(cfg))
			}
		})
	}
}

func TestNextChoice_Wraps(t *testing.T) {
	cases := []struct {
		name    string
		choices []string
		current string
		want    string
	}{
		{name: "steps forward", choices: []string{"mkv", "mp4", "ts"}, current: "mkv", want: "mp4"},
		{name: "wraps at the end", choices: []string{"mkv", "mp4", "ts"}, current: "ts", want: "mkv"},
		{
			name:    "a value not in the set starts at the beginning",
			choices: []string{"mkv", "mp4", "ts"},
			current: "avi",
			want:    "mkv",
		},
		{name: "no choices at all", choices: nil, current: "mkv", want: "mkv"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextChoice(tc.choices, tc.current); got != tc.want {
				t.Errorf("nextChoice() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplySettings_SurvivesAReloadWhileTheEditorIsOpen(t *testing.T) {
	// An edit closes over the channel it was opened on. Update routes keys
	// to the editor while it is open but does not gate a config read, so a
	// reload landing underneath leaves the closure holding a position in a
	// list that is now shorter. Committing it then indexes past the end and
	// takes the whole calendar down, along with every unsaved edit.
	model := everyPane(t)
	press(t, model, "4")

	loaded := model.editor
	loaded.Channels = []config.Channel{
		{Name: "one", Platform: "twitch"},
		{Name: "two", Platform: "twitch"},
		{Name: "three", Platform: "twitch"},
	}
	model.applySettings(settingsMsg{config: loaded})
	_ = model.View()

	// Open an editor on the last channel, which is the one a shorter
	// config will not have.
	last := len(model.editor.Channels) - 1
	model.field = len(model.fields) + last
	model.beginEdit(model.editor.Channels[last].Name, func(value string) error {
		model.editChannel(last, func(channel *config.Channel) { channel.Name = value })
		return nil
	})
	if !model.editing {
		t.Fatal("the editor did not open")
	}

	shorter := loaded
	shorter.Channels = []config.Channel{{Name: "one", Platform: "twitch"}}
	model.applySettings(settingsMsg{config: shorter})

	if model.editing {
		t.Error("the editor stayed open over a config it was not opened on")
	}

	// Whatever the pane does next, it must not take the process with it.
	model.commitEdit()
	press(t, model, "enter")
	_ = model.View()

	if rows := model.rows(); rows > 0 && model.field >= rows {
		t.Errorf("field = %d against %d rows, want it clamped inside the list", model.field, rows)
	}
}

func TestHandleKey_AsksBeforeQuittingOnUnsavedEdits(t *testing.T) {
	// The settings pane is the one place holding work that exists nowhere
	// else, and its own chip set advertises save and quit side by side.
	// Quitting straight out of it drops every edit with nothing said.
	model := everyPane(t)
	press(t, model, "4")

	loaded := model.editor
	loaded.Channels = []config.Channel{{Name: "one", Platform: "twitch"}}
	model.applySettings(settingsMsg{config: loaded})
	model.settle()

	if cmd := model.handleKey("q"); cmd == nil {
		t.Error("q returned no command, want the warning toast")
	}
	if model.quit {
		t.Fatal("q quit with unsaved edits on the first press")
	}

	// A second press goes through, so an operator who meant it is not made
	// to hunt for another key.
	model.handleKey("q")
	if !model.quit {
		t.Error("a second q did not quit")
	}
}

func TestHandleKey_QuitsWithNothingUnsaved(t *testing.T) {
	model := everyPane(t)

	model.handleKey("q")
	if !model.quit {
		t.Error("q did not quit with no unsaved edits")
	}
}
