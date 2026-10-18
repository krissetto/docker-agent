#include <pebble.h>
#include "message_keys.auto.h"
#include "protocol.h"
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define MAX_ROWS 10
#define MAX_FRAME_ROWS 10
#define ROW_ID_CAP 41
#define ROLE_CAP 20
#define OWNER_CAP 25
#define ROW_TEXT_CAP 193
#define META_CAP 49
#define NAME_CAP 49
#define SOURCE_CAP 49
#define CURSOR_CAP 193
#define DRAFT_CAP 385
#define READER_CAP 2049
#define BODY_CAP 1536
#define REQUEST_TIMEOUT_MS 10000
#define SUPPORTED_INBOX_MIN 1024
#define APP_OUTBOX_TARGET 1024

enum Screen { SCREEN_RUNTIMES, SCREEN_AGENTS, SCREEN_SESSIONS, SCREEN_CHAT,
              SCREEN_READER, SCREEN_REVIEW, SCREEN_STATUS, SCREEN_VOICE_ERROR, SCREEN_MENU, SCREEN_ERROR };
enum MenuKind { MENU_NONE, MENU_REVIEW, MENU_COMPOSE, MENU_CANNED, MENU_VOICE_ERROR };
enum LinkState { LINK_CONNECTING, LINK_READY, LINK_OFFLINE, LINK_UNAUTHORIZED,
                 LINK_UNSUPPORTED, LINK_BUSY };
enum AlertMode { ALERT_HAPTIC = 0, ALERT_SOUND_HAPTIC = 1, ALERT_SILENT = 2 };
typedef struct {
  char id[ROW_ID_CAP];
  char role[ROLE_CAP];
  char owner[OWNER_CAP];
  char text[ROW_TEXT_CAP];
  char meta[META_CAP];
  uint8_t kind;
  bool truncated;
} ViewRow;
typedef struct {
  char status[NAME_CAP];
  char error[META_CAP];
  char error_code[ROLE_CAP];
  char runtime_id[ROW_ID_CAP];
  char runtime_name[NAME_CAP];
  char source[SOURCE_CAP];
  char agent_name[NAME_CAP];
  char session_id[ROW_ID_CAP];
  char session_title[NAME_CAP];
  char runtime_status[NAME_CAP];
  char state[ROLE_CAP];
  char before_cursor[CURSOR_CAP];
  char disposition[ROLE_CAP];
  char voice_state[ROLE_CAP];
  uint8_t alert_mode;
  uint8_t alert;
  uint16_t pending;
  uint16_t approval_count;
  uint16_t elicitation_count;
  bool attention_known;
  bool execution_available;
  bool truncated_before;
} ViewMeta;

static Window *s_window;
static TextLayer *s_title, *s_body, *s_footer;
static Layer *s_band;
static DictationSession *s_dictation;
static AppTimer *s_poll_timer, *s_request_timer;
static ViewRow s_rows[MAX_ROWS], s_staged_rows[MAX_FRAME_ROWS], s_merge_rows[MAX_ROWS], s_new_rows[MAX_FRAME_ROWS];
static ViewMeta s_view, s_staged_view;
static uint8_t s_row_count, s_staged_count, s_selected, s_new_count, s_new_row_count;
static uint32_t s_old_attention_count;
static uint32_t s_request_id, s_active_request, s_active_command, s_frame_total, s_next_frame;
static uint8_t s_frame_row_limit, s_frame_row_kind;
static uint32_t s_inbox_capacity, s_outbox_capacity;
static uint32_t s_generation;
static uint8_t s_last_frame_kind;
static uint32_t s_last_frame_index;
static bool s_have_last_frame, s_busy, s_focused = true, s_stage_active, s_sending_ack;
static bool s_attention_baseline, s_sent_from_watch, s_completion_alert_pending, s_first_baseline = true;
static bool s_attention_known, s_attention_active, s_running, s_execution_available;
static enum Screen s_screen = SCREEN_RUNTIMES;
static enum LinkState s_link = LINK_CONNECTING;
static char s_draft[DRAFT_CAP], s_draft_key[UUID_STRING_BUFFER_LENGTH];
static char s_voice_error[META_CAP], s_reader[READER_CAP], s_new_cursor[CURSOR_CAP];
static char s_render_buffer[BODY_CAP];
static uint16_t s_reader_offset;
static uint8_t s_alert_mode = ALERT_HAPTIC;
static enum MenuKind s_menu_kind;
static uint8_t s_menu_count, s_menu_selected;
static bool s_send_unconfirmed, s_reader_chat_return = true;
static enum Screen s_error_return_screen = SCREEN_RUNTIMES;
static bool s_polling_enabled = true;
static uint8_t s_poll_failures;
static uint32_t s_last_activity_ms, s_poll_window_started;
static uint32_t s_touch_down_at, s_last_layout_at, s_last_alert_at;
static int16_t s_touch_x, s_touch_y, s_touch_last_y;
static bool s_touch_active, s_touch_moved;
static bool s_loading_older;

static void render(void);
static void send_poll(void);
static void poll_callback(void *context);
static void start_dictation(void);
static void open_review_menu(void);
static void open_compose_menu(void);
static void open_canned_menu(void);
static void open_voice_menu(void);
static void menu_action_selected(void);
static void mark_user_activity(void);
static void select_action(void);
static void go_back(void);

static uint32_t monotonic_ms(void) {
  uint16_t millis = 0;
  time_t seconds = time(NULL);
  time_ms(&seconds, &millis);
  return (uint32_t)seconds * 1000u + millis;
}

static bool copy_exact(char *destination, size_t capacity, const char *source) {
  if (!capacity) return false;
  if (!source) source = "";
  size_t length = strlen(source);
  if (length >= capacity) return false;
  memcpy(destination, source, length + 1);
  return true;
}

static bool tuple_string(DictionaryIterator *iter, uint32_t key, char *out, size_t cap, bool required) {
  Tuple *tuple = dict_find(iter, key);
  if (!tuple) {
    if (cap) out[0] = '\0';
    return !required;
  }
  if (tuple->type != TUPLE_CSTRING || !copy_exact(out, cap, tuple->value->cstring)) return false;
  return true;
}

static bool tuple_uint(DictionaryIterator *iter, uint32_t key, uint32_t *value, bool required) {
  Tuple *tuple = dict_find(iter, key);
  if (!tuple) return !required;
  if (tuple->type != TUPLE_UINT) return false;
  *value = tuple->value->uint32;
  return true;
}

static bool tuple_bool(DictionaryIterator *iter, uint32_t key, bool *value, bool required) {
  Tuple *tuple = dict_find(iter, key);
  if (!tuple) return !required;
  if (tuple->type == TUPLE_UINT) *value = tuple->value->uint32 != 0;
  else if (tuple->type == TUPLE_INT) *value = tuple->value->int32 != 0;
  else return false;
  return true;
}

static bool put_uint(DictionaryIterator *iter, uint32_t key, uint32_t value) {
  return dict_write_uint32(iter, key, value) == DICT_OK;
}

static bool put_string(DictionaryIterator *iter, uint32_t key, const char *value) {
  return !value || !*value || dict_write_cstring(iter, key, value) == DICT_OK;
}

static void stop_request_timer(void) {
  if (s_request_timer) app_timer_cancel(s_request_timer);
  s_request_timer = NULL;
}

static void clear_stage(void) {
  s_stage_active = false;
  s_staged_count = 0;
  s_frame_row_limit = 0; s_frame_row_kind = UINT8_MAX;
  memset(&s_staged_view, 0, sizeof(s_staged_view));
  memset(s_staged_rows, 0, sizeof(s_staged_rows));
}

static void request_timeout(void *context) {
  (void)context;
  s_request_timer = NULL;
  if (!s_busy) return;
  s_busy = false;
  clear_stage(); s_loading_older = false;
  s_link = LINK_OFFLINE;
  s_send_unconfirmed = s_active_command == AGENT_CMD_SEND || s_active_command == AGENT_CMD_START_NEW;
  copy_exact(s_voice_error, sizeof(s_voice_error), "Not confirmed · Retry keeps this draft key");
  render();
}

static void arm_request_timer(void) {
  stop_request_timer();
  s_request_timer = app_timer_register(REQUEST_TIMEOUT_MS, request_timeout, NULL);
}

static bool begin_packet(DictionaryIterator **iter, uint32_t request, uint32_t type) {
  if (!connection_service_peek_pebble_app_connection()) {
    s_link = LINK_OFFLINE;
    copy_exact(s_voice_error, sizeof(s_voice_error), "Phone disconnected");
    render();
    return false;
  }
  if (app_message_outbox_begin(iter) != APP_MSG_OK) return false;
  if (!put_uint(*iter, MESSAGE_KEY_type, type) ||
      !put_uint(*iter, MESSAGE_KEY_request_id, request)) return false;
  return true;
}

static uint32_t next_request_id(void) {
  s_request_id++;
  if (!s_request_id) s_request_id++;
  return s_request_id;
}

static bool finish_packet(bool start_wait) {
  AppMessageResult result = app_message_outbox_send();
  if (result != APP_MSG_OK) {
    if (start_wait) s_busy = false;
    if (!start_wait) s_sending_ack = false;
    s_link = result == APP_MSG_NOT_CONNECTED ? LINK_OFFLINE : LINK_BUSY;
    render();
    return false;
  }
  if (start_wait) {
    s_busy = true;
    s_active_request = s_request_id;
    s_active_command = 0;
    s_have_last_frame = false;
    clear_stage();
    arm_request_timer();
  }
  return true;
}

static bool send_command(uint32_t type, const char *runtime_id, const char *agent_name,
                         const char *source, const char *session_id, const char *cursor,
                         const char *text, const char *draft_key) {
  if (s_busy) return false;
  DictionaryIterator *iter;
  uint32_t request = next_request_id();
  if (!begin_packet(&iter, request, type)) return false;
  bool ok = true;
  if (type != AGENT_CMD_ACK && type != AGENT_CMD_CANCEL)
    ok = put_uint(iter, MESSAGE_KEY_receive_capacity, s_inbox_capacity);
  if (ok && type == AGENT_CMD_SELECT && runtime_id) ok = put_string(iter, MESSAGE_KEY_runtime_id, runtime_id);
  if (ok && (type == AGENT_CMD_SELECT || type == AGENT_CMD_START_NEW) && agent_name)
    ok = put_string(iter, MESSAGE_KEY_agent_name, agent_name);
  if (ok && (type == AGENT_CMD_OPEN_SESSION || type == AGENT_CMD_SEND ||
      type == AGENT_CMD_POLL || type == AGENT_CMD_OLDER) && session_id)
    ok = put_string(iter, MESSAGE_KEY_session_id, session_id);
  if (ok && type == AGENT_CMD_OLDER && cursor)
    ok = put_string(iter, MESSAGE_KEY_before_cursor, cursor);
  if (ok && (type == AGENT_CMD_SELECT || type == AGENT_CMD_START_NEW) && source)
    ok = put_string(iter, MESSAGE_KEY_source, source);
  if (ok && type == AGENT_CMD_START_NEW && source && strcmp(source, s_view.source)) {
    s_link = LINK_UNSUPPORTED; render(); return false;
  }
  if (ok && (type == AGENT_CMD_START_NEW || type == AGENT_CMD_SEND) && text)
    ok = put_string(iter, MESSAGE_KEY_text, text);
  if (ok && (type == AGENT_CMD_START_NEW || type == AGENT_CMD_SEND || type == AGENT_CMD_CANCEL) && draft_key)
    ok = put_string(iter, MESSAGE_KEY_draft_key, draft_key);
  if (!ok) { s_link = LINK_UNSUPPORTED; render(); return false; }
  bool sent = finish_packet(true);
  if (sent) s_active_command = type;
  return sent;
}

static bool send_ack(uint32_t request, uint32_t frame, uint32_t index) {
  DictionaryIterator *iter;
  if (!begin_packet(&iter, request, AGENT_CMD_ACK)) return false;
  if (!put_uint(iter, MESSAGE_KEY_frame, frame) || !put_uint(iter, MESSAGE_KEY_index, index)) return false;
  s_sending_ack = true;
  return finish_packet(false);
}

static void poll_callback(void *context) { (void)context; s_poll_timer = NULL; send_poll(); }

static void send_poll(void) {
  if (s_busy || !s_focused) return;
  s_last_activity_ms = s_last_activity_ms ? s_last_activity_ms : monotonic_ms();
  if (!s_poll_window_started) s_poll_window_started = monotonic_ms();
  if (s_screen == SCREEN_ERROR) {
    if (s_link == LINK_UNAUTHORIZED || s_link == LINK_UNSUPPORTED || s_poll_failures >= 3) return;
    s_screen = s_error_return_screen;
  }
  if (!connection_service_peek_pebble_app_connection()) {
    s_link = LINK_OFFLINE;
    copy_exact(s_voice_error, sizeof(s_voice_error), "Phone disconnected · reconnect to refresh");
    render();
    return;
  }
  if (s_view.session_id[0] && s_screen != SCREEN_RUNTIMES && s_screen != SCREEN_AGENTS && s_screen != SCREEN_SESSIONS)
    send_command(AGENT_CMD_POLL, NULL, NULL, NULL, s_view.session_id, NULL, NULL, NULL);
  else if (s_screen == SCREEN_CHAT && !s_view.session_id[0]) return;
  else if (s_screen == SCREEN_AGENTS && s_view.runtime_id[0])
    send_command(AGENT_CMD_SELECT, s_view.runtime_id, NULL, NULL, NULL, NULL, NULL, NULL);
  else if (s_screen == SCREEN_SESSIONS && s_view.agent_name[0])
    send_command(AGENT_CMD_SELECT, NULL, s_view.agent_name, s_view.source, NULL, NULL, NULL, NULL);
  else
    send_command(AGENT_CMD_HELLO, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
}

static void schedule_poll(uint32_t delay) {
  if (s_poll_timer) app_timer_cancel(s_poll_timer);
  s_poll_timer = NULL;
  if (!s_focused || !s_polling_enabled) return;
  uint32_t now = monotonic_ms();
  if (s_screen == SCREEN_CHAT && now - s_last_activity_ms >= 600000) return;
  if ((s_screen == SCREEN_RUNTIMES || s_screen == SCREEN_AGENTS || s_screen == SCREEN_SESSIONS) &&
      now - s_poll_window_started >= 300000) return;
  s_poll_timer = app_timer_register(delay, poll_callback, NULL);
}

static const char *link_word(void) {
  if (s_link == LINK_OFFLINE) return "OFFLINE";
  if (s_link == LINK_UNAUTHORIZED) return "! SIGN IN";
  if (s_link == LINK_UNSUPPORTED) return "! UNSUPPORTED";
  if (s_busy) return "Sending…";
  if (s_attention_active) return "! NEEDS YOU";
  if (s_running) return "WORKING";
  if (!strcmp(s_view.state, "settled")) return "DONE";
  if (!strcmp(s_view.state, "unknown")) return "STATUS UNKNOWN";
  if (!strcmp(s_view.runtime_status, "offline")) return "RUNTIME OFFLINE";
  if (s_link == LINK_CONNECTING) return "CONNECTING";
  return "IDLE";
}

static GColor band_color(void) {
  if (s_link == LINK_OFFLINE || s_link == LINK_UNAUTHORIZED || s_link == LINK_UNSUPPORTED) return GColorBlack;
  if (s_attention_active) return GColorChromeYellow;
  if (s_running || s_busy) return GColorCobaltBlue;
  return GColorWhite;
}

static void band_draw(Layer *layer, GContext *ctx) {
  GRect bounds = layer_get_bounds(layer);
  GColor fill = band_color();
  GColor ink = (s_link == LINK_OFFLINE || s_link == LINK_UNAUTHORIZED || s_link == LINK_UNSUPPORTED || s_running || s_busy) ? GColorWhite : GColorBlack;
  graphics_context_set_fill_color(ctx, fill);
  graphics_fill_rect(ctx, bounds, 0, GCornerNone);
  graphics_context_set_text_color(ctx, ink);
  char line[64];
  snprintf(line, sizeof(line), "%s", link_word());
  graphics_draw_text(ctx, line, fonts_get_system_font(FONT_KEY_GOTHIC_18_BOLD),
    GRect(6, 0, bounds.size.w - 12, bounds.size.h), GTextOverflowModeTrailingEllipsis, GTextAlignmentLeft, NULL);
}

static void set_body(const char *text) {
  if (s_body) text_layer_set_text(s_body, text ? text : "");
}

static void set_title(const char *text) {
  if (s_title) text_layer_set_text(s_title, text ? text : "docker-agent");
}

static const char *screen_target(void) {
  if (s_view.agent_name[0] && s_view.runtime_name[0]) {
    static char target[104];
    snprintf(target, sizeof(target), "%s · %s", s_view.agent_name, s_view.runtime_name);
    return target;
  }
  return s_view.runtime_name[0] ? s_view.runtime_name : "Choose a runtime";
}

#define APPEND_FORMAT(buffer, capacity, ...) do { \
  size_t append_used = strlen(buffer); \
  if (append_used < (capacity)) snprintf((buffer) + append_used, (capacity) - append_used, __VA_ARGS__); \
} while (0)

static void render_list(void) {
  char *body = s_render_buffer;
  body[0] = '\0';
  uint8_t first = s_selected > 2 ? (uint8_t)(s_selected - 2) : 0;
  if (s_screen == SCREEN_SESSIONS) {
    APPEND_FORMAT(body, BODY_CAP, "%s + New chat\n", s_selected == 0 ? ">" : " ");
    uint8_t first_session = s_selected > 2 ? (uint8_t)(s_selected - 3) : 0;
    for (uint8_t visible = 0; visible < 3 && first_session + visible < s_row_count; visible++) {
      uint8_t i = (uint8_t)(first_session + visible);
      ViewRow *row = &s_rows[i];
      APPEND_FORMAT(body, BODY_CAP, "%s %s\n  %s\n", s_selected == i + 1 ? ">" : " ",
        row->text[0] ? row->text : row->id, row->meta);
    }
    APPEND_FORMAT(body, BODY_CAP, "Older on phone or desktop");
    set_title(s_view.agent_name[0] ? s_view.agent_name : "Sessions");
    text_layer_set_text(s_footer, "Select session · New chat");
  } else if (s_screen == SCREEN_AGENTS) {
    set_title(s_view.runtime_name[0] ? s_view.runtime_name : "Root agents");
    for (uint8_t i = 0; i < 4 && first + i < s_row_count; i++) {
      ViewRow *row = &s_rows[first + i];
      APPEND_FORMAT(body, BODY_CAP, "%s %s\n  %s · %s\n", first + i == s_selected ? ">" : " ",
        row->text[0] ? row->text : row->id, row->meta, row->role);
    }
    if (!s_row_count) APPEND_FORMAT(body, BODY_CAP, "No root agents available.");
    text_layer_set_text(s_footer, "Choose root agent");
  } else {
    set_title("Runtimes · phone config");
    for (uint8_t i = 0; i < 4 && first + i < s_row_count; i++) {
      ViewRow *row = &s_rows[first + i];
      const char *state = row->role[0] ? row->role : "status unknown";
      APPEND_FORMAT(body, BODY_CAP, "%s %s\n  %s · %s\n", first + i == s_selected ? ">" : " ",
        row->text[0] ? row->text : row->id, state, row->meta);
    }
    if (!s_row_count) APPEND_FORMAT(body, BODY_CAP, "No runtimes configured.\nOpen Pebble app settings on your phone.");
    text_layer_set_text(s_footer, s_row_count ? "Choose runtime" : "Add runtime on phone");
  }
  set_body(body);
}

static void render_chat(void) {
  char *body = s_render_buffer;
  body[0] = '\0';
  APPEND_FORMAT(body, BODY_CAP, "%s\n", screen_target());
  if (s_attention_active) {
    APPEND_FORMAT(body, BODY_CAP, "! %u approval · %u question\nAnswer on phone or desktop\n",
      s_view.approval_count, s_view.elicitation_count);
  } else if (!s_attention_known) {
    APPEND_FORMAT(body, BODY_CAP, "Attention unknown\n");
  }
  if (!s_row_count) APPEND_FORMAT(body, BODY_CAP, "No recent messages\n");
  uint8_t from = s_selected > 1 ? (uint8_t)(s_selected - 1) : 0;
  uint8_t shown = 0;
  for (uint8_t i = from; i < s_row_count && shown < 3; i++, shown++) {
    ViewRow *row = &s_rows[i];
    const char *owner = !strcmp(row->role, "user") ? "You" : (row->owner[0] ? row->owner : s_view.agent_name);
    APPEND_FORMAT(body, BODY_CAP, "%s %s\n%s%s\n%s%s\n", i == s_selected ? ">" : " ", owner,
      row->text, row->truncated ? " …" : "", row->meta[0] ? row->meta : "", row->meta[0] ? "\n" : "");
  }
  if (s_selected >= s_row_count) APPEND_FORMAT(body, BODY_CAP, "> Reply...%s\n", s_draft[0] ? " · Draft ready" : "");
  if (s_new_count) APPEND_FORMAT(body, BODY_CAP, "> %u NEW · DOWN TO JUMP\n", s_new_count);
  if (s_view.truncated_before) APPEND_FORMAT(body, BODY_CAP, "Earlier turns · Up for older\n");
  if (!s_execution_available) APPEND_FORMAT(body, BODY_CAP, "History only · can't send\n");
  set_title(s_view.session_title[0] ? s_view.session_title : "Chat");
  set_body(body);
  text_layer_set_text(s_footer, s_link == LINK_OFFLINE ? "Phone disconnected" :
    (s_running ? "Queues after current turn" : "Select row · hold Select voice"));
}

static void render_reader(void) {
  set_title(s_reader_chat_return ? (s_view.agent_name[0] ? s_view.agent_name : "Message") : "Message");
  set_body(s_reader + s_reader_offset);
  text_layer_set_text(s_footer, "Up / Down scroll · Back");
}

static void render_review(void) {
  char *body = s_render_buffer;
  snprintf(body, BODY_CAP, "%s\n%s · %s\n\n%s", s_view.session_id[0] ? "Reply to" : "New chat",
    s_view.agent_name[0] ? s_view.agent_name : "Pick an agent", s_view.runtime_name[0] ? s_view.runtime_name : "Runtime", s_draft);
  set_title("Review · explicit Send");
  set_body(body);
  text_layer_set_text(s_footer, "Select: Send options · Back keeps draft");
}

static void render_status(void) {
  char *body = s_render_buffer;
  snprintf(body, BODY_CAP, "%s\n%s · %s\n%s\n%u queued · %u approvals · %u questions\nHeap %lu B · %s\nAlert: %s%s",
    link_word(), s_view.agent_name[0] ? s_view.agent_name : "No root agent", s_view.runtime_name[0] ? s_view.runtime_name : "No runtime",
    s_view.state[0] ? s_view.state : (s_view.runtime_status[0] ? s_view.runtime_status : "Status unknown"), s_view.pending,
    s_attention_known ? s_view.approval_count : 0, s_attention_known ? s_view.elicitation_count : 0,
    (unsigned long)heap_bytes_free(), s_view.before_cursor[0] ? "Older history available" : "Recent view",
    s_alert_mode == ALERT_SILENT ? "Silent" : (s_alert_mode == ALERT_SOUND_HAPTIC ? "Sound + haptic" : "Haptic"),
#ifdef PBL_SPEAKER
    speaker_is_muted() ? " · watch muted" : ""
#else
    ""
#endif
  );
  set_title("Status"); set_body(body); text_layer_set_text(s_footer, "Back");
}

static const char *menu_label(uint8_t index) {
  if (s_menu_kind == MENU_REVIEW) {
    if (index == 0) return s_send_unconfirmed ? "Retry · same draft" : (s_view.session_id[0] ? "Send to agent" : "Start root chat");
    if (index == 1) return "Re-record voice";
    if (index == 2) return "Choose canned reply";
    return "Discard draft";
  }
  if (s_menu_kind == MENU_COMPOSE) return index == 0 ? "Voice · dictate" : "Canned replies";
  if (s_menu_kind == MENU_CANNED) {
    static const char *const replies[] = { "Status update?", "Continue.", "Summarize where you are.",
      "Pause after this step", "Looks good, thanks." };
    return replies[index];
  }
  if (s_menu_kind == MENU_VOICE_ERROR) return index == 0 ? "Try voice again" : index == 1 ? "Canned replies" : "Back to chat";
  return "Back";
}

static void render_menu(void) {
  char *body = s_render_buffer; body[0] = '\0';
  for (uint8_t i = 0; i < s_menu_count; i++) APPEND_FORMAT(body, BODY_CAP, "%s %s\n", i == s_menu_selected ? ">" : " ", menu_label(i));
  set_title(s_menu_kind == MENU_REVIEW ? "Review actions" : s_menu_kind == MENU_COMPOSE ? "Compose reply" :
    s_menu_kind == MENU_CANNED ? "Canned replies" : "Voice unavailable");
  set_body(body); text_layer_set_text(s_footer, "Tap to highlight · Select to choose");
}

static void render_error(void) {
  char *body = s_render_buffer;
  snprintf(body, BODY_CAP, "%s\n\n%s\n\n%s", s_link == LINK_UNAUTHORIZED ? "Sign in on phone" :
    s_link == LINK_UNSUPPORTED ? "Bridge unsupported" : "Not sent / offline",
    s_voice_error[0] ? s_voice_error : "Phone bridge could not confirm this action.",
    s_draft[0] ? "Draft kept · Select Retry" : "Back to the previous view");
  set_title("Connection / send error"); set_body(body);
  text_layer_set_text(s_footer, "Select retry · Back keeps cached view");
}

static void render_voice_error(void) {
  char *body = s_render_buffer;
  snprintf(body, BODY_CAP, "%s\n\n%s\n\nUse canned reply or try again.",
    s_voice_error[0] ? "Voice unavailable" : "Voice failed", s_voice_error);
  set_title("Voice · retry or canned"); set_body(body);
  text_layer_set_text(s_footer, "Select actions · Back");
}

static void render(void) {
  if (!s_body) return;
  switch (s_screen) {
    case SCREEN_RUNTIMES: case SCREEN_AGENTS: case SCREEN_SESSIONS: render_list(); break;
    case SCREEN_CHAT: render_chat(); break;
    case SCREEN_READER: render_reader(); break;
    case SCREEN_REVIEW: render_review(); break;
    case SCREEN_STATUS: render_status(); break;
    case SCREEN_VOICE_ERROR: render_voice_error(); break;
    case SCREEN_MENU: render_menu(); break;
    case SCREEN_ERROR: render_error(); break;
  }
  if (s_band) layer_mark_dirty(s_band);
  s_last_layout_at = monotonic_ms();
}

static void create_draft_key(void) {
  Uuid uuid = UuidMake((uint8_t)(time(NULL) >> 24), (uint8_t)(time(NULL) >> 16),
    (uint8_t)(time(NULL) >> 8), (uint8_t)time(NULL), (uint8_t)(++s_generation >> 24),
    (uint8_t)(s_generation >> 16), (uint8_t)(s_generation >> 8), (uint8_t)s_generation,
    (uint8_t)rand(), (uint8_t)rand(), (uint8_t)rand(), (uint8_t)rand(),
    (uint8_t)rand(), (uint8_t)rand(), (uint8_t)rand(), (uint8_t)rand());
  char formatted[UUID_STRING_BUFFER_LENGTH];
  uuid_to_string(&uuid, formatted);
  uint8_t output = 0;
  for (size_t i = 1; formatted[i] && formatted[i] != '}'; i++) {
    if (formatted[i] != '-') s_draft_key[output++] = formatted[i];
  }
  s_draft_key[output] = '\0';
}

static void reset_draft(void) {
  s_draft[0] = '\0';
  s_draft_key[0] = '\0';
  s_send_unconfirmed = false;
}

static void send_draft(void) {
  if (!s_draft[0]) return;
  if (!s_draft_key[0]) create_draft_key();
  bool is_new = !s_view.session_id[0];
  if (is_new && (!s_view.agent_name[0] || !s_view.runtime_id[0])) {
    copy_exact(s_voice_error, sizeof(s_voice_error), "Choose a runtime and exact root agent before starting a chat.");
    s_screen = SCREEN_ERROR; render(); return;
  }
  if (!is_new && !s_execution_available) {
    copy_exact(s_voice_error, sizeof(s_voice_error), "History only · this root cannot accept messages");
    s_screen = SCREEN_REVIEW; render(); return;
  }
  s_screen = SCREEN_CHAT;
  s_last_activity_ms = monotonic_ms();
  s_sent_from_watch = true;
  s_send_unconfirmed = true;
  if (is_new) send_command(AGENT_CMD_START_NEW, NULL, s_view.agent_name, s_view.source, NULL, NULL, s_draft, s_draft_key);
  else send_command(AGENT_CMD_SEND, NULL, NULL, NULL, s_view.session_id, NULL, s_draft, s_draft_key);
  render();
}

static void discard_draft(void) {
  reset_draft();
  s_screen = s_view.session_id[0] ? SCREEN_CHAT : SCREEN_SESSIONS;
  render();
}

static void open_menu(enum MenuKind kind, uint8_t count) {
  s_menu_kind = kind; s_menu_count = count; s_menu_selected = 0;
  s_screen = SCREEN_MENU; render();
}

static void open_review_menu(void) { open_menu(MENU_REVIEW, 4); }
static void open_compose_menu(void) { open_menu(MENU_COMPOSE, 2); }
static void open_canned_menu(void) { open_menu(MENU_CANNED, 5); }
static void open_voice_menu(void) { open_menu(MENU_VOICE_ERROR, 3); }

static void menu_action_selected(void) {
  uint8_t selected = s_menu_selected;
  enum MenuKind kind = s_menu_kind;
  if (kind == MENU_REVIEW) {
    if (selected == 0) send_draft();
    else if (selected == 1) start_dictation();
    else if (selected == 2) open_canned_menu();
    else discard_draft();
  } else if (kind == MENU_COMPOSE) {
    if (selected == 0) start_dictation(); else open_canned_menu();
  } else if (kind == MENU_CANNED) {
    static const char *const replies[] = { "Status update?", "Continue.", "Summarize where you are.",
      "Pause after this step and wait for me.", "Looks good, thanks." };
    copy_exact(s_draft, sizeof(s_draft), replies[selected]);
    create_draft_key(); s_screen = SCREEN_REVIEW; render();
  } else if (kind == MENU_VOICE_ERROR) {
    if (selected == 0) start_dictation();
    else if (selected == 1) open_canned_menu();
    else { s_screen = s_view.session_id[0] ? SCREEN_CHAT : SCREEN_SESSIONS; render(); }
  }
}

static void dictation_callback(DictationSession *session, DictationSessionStatus status, char *text, void *context) {
  (void)session; (void)context;
  if (status == DictationSessionStatusSuccess && text && *text) {
    if (!copy_exact(s_draft, sizeof(s_draft), text)) {
      copy_exact(s_voice_error, sizeof(s_voice_error), "Voice text exceeds the draft limit. Try a shorter reply.");
      s_screen = SCREEN_VOICE_ERROR; render(); return;
    }
    create_draft_key(); s_screen = SCREEN_REVIEW; render(); return;
  }
  if (status == DictationSessionStatusFailureSystemAborted || status == DictationSessionStatusFailureTranscriptionRejected) {
    s_screen = s_draft[0] ? SCREEN_REVIEW : (s_view.session_id[0] ? SCREEN_CHAT : SCREEN_SESSIONS);
    render(); return;
  }
  if (status == DictationSessionStatusFailureDisabled)
    copy_exact(s_voice_error, sizeof(s_voice_error), "Voice isn't set up or is off in Pebble app settings.");
  else if (status == DictationSessionStatusFailureConnectivityError)
    copy_exact(s_voice_error, sizeof(s_voice_error), "Voice needs phone Internet.");
  else if (status == DictationSessionStatusFailureNoSpeechDetected)
    copy_exact(s_voice_error, sizeof(s_voice_error), "Didn't hear anything.");
  else if (status == DictationSessionStatusFailureInternalError || status == DictationSessionStatusFailureRecognizerError)
    copy_exact(s_voice_error, sizeof(s_voice_error), "Voice failed. Retry or use canned replies.");
  else copy_exact(s_voice_error, sizeof(s_voice_error), "Voice unavailable. Check phone speech settings.");
  s_screen = SCREEN_VOICE_ERROR; render();
}

static void start_dictation(void) {
  if (!connection_service_peek_pebble_app_connection()) {
    copy_exact(s_voice_error, sizeof(s_voice_error), "Voice needs your phone connected.");
    s_screen = SCREEN_VOICE_ERROR; render(); return;
  }
  if (!s_dictation) s_dictation = dictation_session_create(sizeof(s_draft), dictation_callback, NULL);
  if (!s_dictation) {
    copy_exact(s_voice_error, sizeof(s_voice_error), "This watch cannot open Dictation.");
    s_screen = SCREEN_VOICE_ERROR; render(); return;
  }
  dictation_session_enable_confirmation(s_dictation, true);
  dictation_session_enable_error_dialogs(s_dictation, true);
  DictationSessionStatus result = dictation_session_start(s_dictation);
  if (result != DictationSessionStatusSuccess) {
    copy_exact(s_voice_error, sizeof(s_voice_error), "Voice could not start. Retry or use canned replies.");
    s_screen = SCREEN_VOICE_ERROR; render();
  }
}

static void alert_user(bool attention) {
  uint32_t now = monotonic_ms();
  if (quiet_time_is_active() || s_alert_mode == ALERT_SILENT ||
      (s_last_alert_at && now - s_last_alert_at < 30000)) return;
  s_last_alert_at = now;
#ifdef PBL_SPEAKER
  if (s_alert_mode == ALERT_SOUND_HAPTIC && !speaker_is_muted()) {
    if (attention) {
      static const SpeakerNote notes[] = {
        { .midi_note = 72, .waveform = SpeakerWaveformSine, .duration_ms = 90, .velocity = 68 },
        { .midi_note = 79, .waveform = SpeakerWaveformSine, .duration_ms = 100, .velocity = 68 }
      };
      speaker_play_notes(notes, 2, 68);
    } else {
      speaker_play_tone(880, 100, 68, SpeakerWaveformSine);
    }
  }
#endif
  if (attention) vibes_double_pulse();
  else vibes_short_pulse();
}

static bool parse_meta(DictionaryIterator *iter, ViewMeta *meta) {
  uint32_t number = 0;
  char view_status[sizeof(meta->status)];
  if (!copy_exact(view_status, sizeof(view_status), meta->status)) return false;
  memset(meta, 0, sizeof(*meta));
  copy_exact(meta->status, sizeof(meta->status), view_status);
#define READ_STR(field, key, cap, required) if (!tuple_string(iter, MESSAGE_KEY_##key, meta->field, cap, required)) return false
  READ_STR(error_code, error, sizeof(meta->error_code), false);
  char error_text[META_CAP] = "";
  if (!tuple_string(iter, MESSAGE_KEY_text, error_text, sizeof(error_text), false)) return false;
  if (error_text[0] && !strcmp(meta->status, "error")) copy_exact(meta->error, sizeof(meta->error), error_text);
  READ_STR(runtime_id, runtime_id, sizeof(meta->runtime_id), false);
  READ_STR(runtime_name, runtime_name, sizeof(meta->runtime_name), false);
  READ_STR(source, source, sizeof(meta->source), false);
  char alias[ROW_ID_CAP] = "";
  if (!tuple_string(iter, MESSAGE_KEY_agent_name, meta->agent_name, sizeof(meta->agent_name), false) ||
      !tuple_string(iter, MESSAGE_KEY_selected_agent, alias, sizeof(alias), false) ||
      (alias[0] && meta->agent_name[0] && strcmp(alias, meta->agent_name)) ||
      (!meta->agent_name[0] && alias[0] && !copy_exact(meta->agent_name, sizeof(meta->agent_name), alias)) ||
      !tuple_string(iter, MESSAGE_KEY_session_id, meta->session_id, sizeof(meta->session_id), false) ||
      !tuple_string(iter, MESSAGE_KEY_selected_session, alias, sizeof(alias), false) ||
      (alias[0] && meta->session_id[0] && strcmp(alias, meta->session_id)) ||
      (!meta->session_id[0] && alias[0] && !copy_exact(meta->session_id, sizeof(meta->session_id), alias)) ||
      !tuple_string(iter, MESSAGE_KEY_session_title, meta->session_title, sizeof(meta->session_title), false) ||
      !tuple_string(iter, MESSAGE_KEY_runtime_status, meta->runtime_status, sizeof(meta->runtime_status), false)) return false;
  char turn_state[ROLE_CAP] = "";
  if (!tuple_string(iter, MESSAGE_KEY_state, turn_state, sizeof(turn_state), false)) return false;
  if (turn_state[0] && !copy_exact(meta->state, sizeof(meta->state), turn_state)) return false;
  char root_id[ROW_ID_CAP] = "";
  if (!tuple_string(iter, MESSAGE_KEY_root_session_id, root_id, sizeof(root_id), false)) return false;
  if (root_id[0] && meta->session_id[0] && strcmp(root_id, meta->session_id)) return false;
  if (root_id[0] && !meta->session_id[0]) copy_exact(meta->session_id, sizeof(meta->session_id), root_id);
  READ_STR(before_cursor, before_cursor, sizeof(meta->before_cursor), false);
  READ_STR(disposition, disposition, sizeof(meta->disposition), false);
  READ_STR(voice_state, voice_state, sizeof(meta->voice_state), false);
#undef READ_STR
  if (!tuple_bool(iter, MESSAGE_KEY_attention_known, &meta->attention_known, false) ||
      !tuple_bool(iter, MESSAGE_KEY_execution_available, &meta->execution_available, false) ||
      !tuple_bool(iter, MESSAGE_KEY_truncated_before, &meta->truncated_before, false)) return false;
  if (tuple_uint(iter, MESSAGE_KEY_pending, &number, false)) meta->pending = number > UINT16_MAX ? UINT16_MAX : number;
  if (tuple_uint(iter, MESSAGE_KEY_approval_count, &number, false)) meta->approval_count = number > UINT16_MAX ? UINT16_MAX : number;
  if (tuple_uint(iter, MESSAGE_KEY_elicitation_count, &number, false)) meta->elicitation_count = number > UINT16_MAX ? UINT16_MAX : number;
  if (tuple_uint(iter, MESSAGE_KEY_alert_mode, &number, false)) meta->alert_mode = number > ALERT_SILENT ? ALERT_HAPTIC : number;
  if (tuple_uint(iter, MESSAGE_KEY_alert, &number, false)) meta->alert = number > 2 ? 0 : number;
  return true;
}

static bool parse_row(DictionaryIterator *iter, ViewRow *row, uint32_t expected_row) {
  uint32_t kind = UINT32_MAX, row_index = UINT32_MAX;
  memset(row, 0, sizeof(*row));
  if (!tuple_uint(iter, MESSAGE_KEY_row_index, &row_index, true) || row_index != expected_row ||
      !tuple_uint(iter, MESSAGE_KEY_row_kind, &kind, true) || kind > AGENT_ROW_RUNTIME) return false;
  row->kind = (uint8_t)kind;
  if (!tuple_string(iter, MESSAGE_KEY_row_id, row->id, sizeof(row->id), true) ||
      !tuple_string(iter, MESSAGE_KEY_row_role, row->role, sizeof(row->role), false) ||
      !tuple_string(iter, MESSAGE_KEY_row_owner, row->owner, sizeof(row->owner), false) ||
      !tuple_string(iter, MESSAGE_KEY_row_text, row->text, sizeof(row->text), false) ||
      !tuple_string(iter, MESSAGE_KEY_row_meta, row->meta, sizeof(row->meta), false) ||
      !tuple_bool(iter, MESSAGE_KEY_row_truncated, &row->truncated, false)) return false;
  if (row->kind == AGENT_ROW_TURN && strlen(row->id) > 20) return false;
  if ((row->kind == AGENT_ROW_SESSION || row->kind == AGENT_ROW_RUNTIME) && strlen(row->id) > 40) return false;
  return true;
}

static void publish_view(void) {
  uint32_t old_attention = (uint32_t)s_view.approval_count + s_view.elicitation_count;
  bool old_known = s_view.attention_known;
  bool old_running = s_running;
  char selected_id[ROW_ID_CAP] = "";
  bool was_at_tail = s_screen == SCREEN_CHAT && s_selected >= s_row_count;
  bool entering_chat = s_screen != SCREEN_CHAT && !strcmp(s_staged_view.status, "chat");
  if (s_screen == SCREEN_CHAT && s_selected < s_row_count)
    copy_exact(selected_id, sizeof(selected_id), s_rows[s_selected].id);
  uint8_t old_count = s_row_count;
  bool was_older = s_loading_older;
  bool is_error_view = !strcmp(s_staged_view.status, "error");
  bool is_terminal_send_ack = (!strcmp(s_staged_view.status, "accepted") || !strcmp(s_staged_view.status, "queued"));
  char old_newest[ROW_ID_CAP] = "";
  if (old_count) copy_exact(old_newest, sizeof(old_newest), s_rows[old_count - 1].id);
  ViewMeta new_view = s_staged_view;
  bool keep_reading_page = false;
  if (is_error_view) {
    s_link = strstr(new_view.error_code, "unauthorized") || strstr(new_view.error_code, "needs_auth") ? LINK_UNAUTHORIZED :
      strstr(new_view.error_code, "unsupported") || strstr(new_view.error_code, "bridge_unsupported") ? LINK_UNSUPPORTED : LINK_OFFLINE;
    s_error_return_screen = s_screen;
    copy_exact(s_voice_error, sizeof(s_voice_error), new_view.error[0] ? new_view.error : new_view.error_code);
    if (strstr(new_view.error_code, "cursor") || strstr(new_view.error, "cursor"))
      copy_exact(s_voice_error, sizeof(s_voice_error), "Older history expired · retry from newest chat.");
    if (s_active_command == AGENT_CMD_SEND || s_active_command == AGENT_CMD_START_NEW) s_send_unconfirmed = true;
    s_busy = false; s_poll_failures++;
    stop_request_timer(); clear_stage(); s_loading_older = false;
    s_screen = SCREEN_ERROR;
    render();
    if (s_poll_failures < 3 && s_polling_enabled) schedule_poll(s_poll_failures == 1 ? 2000 : s_poll_failures == 2 ? 5000 : 10000);
    return;
  }
  s_poll_failures = 0;
  if (s_screen == SCREEN_CHAT && !was_at_tail && selected_id[0] && !was_older) {
    bool still_present = false;
    for (uint8_t i = 0; i < s_staged_count; i++) if (!strcmp(selected_id, s_staged_rows[i].id)) still_present = true;
    keep_reading_page = !still_present;
  }
  if (keep_reading_page) {
    copy_exact(s_new_cursor, sizeof(s_new_cursor), new_view.before_cursor);
    new_view.truncated_before = s_view.truncated_before;
    uint8_t unseen = 0;
    for (uint8_t i = 0; i < s_staged_count; i++) {
      bool seen = false;
      for (uint8_t j = 0; j < old_count; j++) if (!strcmp(s_staged_rows[i].id, s_rows[j].id)) seen = true;
      if (!seen) unseen++;
    }
    memcpy(s_new_rows, s_staged_rows, s_staged_count * sizeof(ViewRow));
    s_new_row_count = s_staged_count;
    s_new_count = unseen > s_new_count ? unseen : s_new_count;
  } else if (s_loading_older) {
    uint8_t prepend = s_staged_count > MAX_FRAME_ROWS ? MAX_FRAME_ROWS : s_staged_count;
    uint8_t keep = s_row_count < MAX_ROWS - prepend ? s_row_count : MAX_ROWS - prepend;
    if (prepend + keep > MAX_ROWS) keep = MAX_ROWS - prepend;
    memcpy(s_merge_rows, s_staged_rows, prepend * sizeof(ViewRow));
    if (keep) memcpy(s_merge_rows + prepend, s_rows, keep * sizeof(ViewRow));
    s_row_count = (uint8_t)(prepend + keep);
    if (s_row_count) memcpy(s_rows, s_merge_rows, s_row_count * sizeof(ViewRow));
    s_loading_older = false;
  } else if (!keep_reading_page) {
    s_row_count = s_staged_count;
    memcpy(s_rows, s_staged_rows, s_row_count * sizeof(ViewRow));
  }
  if (((is_terminal_send_ack && !s_staged_count) || (s_staged_count == 0 && !strcmp(new_view.status, "chat"))) && s_screen == SCREEN_CHAT) {
    if (new_view.session_id[0]) copy_exact(s_view.session_id, sizeof(s_view.session_id), new_view.session_id);
    if (new_view.runtime_id[0]) copy_exact(s_view.runtime_id, sizeof(s_view.runtime_id), new_view.runtime_id);
    if (new_view.runtime_name[0]) copy_exact(s_view.runtime_name, sizeof(s_view.runtime_name), new_view.runtime_name);
    if (new_view.agent_name[0]) copy_exact(s_view.agent_name, sizeof(s_view.agent_name), new_view.agent_name);
    if (new_view.session_title[0]) copy_exact(s_view.session_title, sizeof(s_view.session_title), new_view.session_title);
    if (new_view.error_code[0]) copy_exact(s_view.error_code, sizeof(s_view.error_code), new_view.error_code);
    if (new_view.state[0]) copy_exact(s_view.state, sizeof(s_view.state), new_view.state);
    if (new_view.runtime_status[0]) copy_exact(s_view.runtime_status, sizeof(s_view.runtime_status), new_view.runtime_status);
    if (new_view.disposition[0]) copy_exact(s_view.disposition, sizeof(s_view.disposition), new_view.disposition);
    copy_exact(s_view.status, sizeof(s_view.status), new_view.status);
    s_view.execution_available = new_view.execution_available;
    s_view.alert_mode = new_view.alert_mode;
  } else s_view = new_view;
  s_execution_available = s_view.execution_available;
  s_alert_mode = s_view.alert_mode;
  s_attention_known = s_view.attention_known;
  s_attention_active = s_attention_known && (s_view.approval_count || s_view.elicitation_count);
  s_running = !strcmp(s_view.state, "running") || !strcmp(s_view.state, "queued") ||
    !strcmp(s_view.state, "cancelling");
  s_link = LINK_READY;
  if (strstr(s_view.error, "unauthorized")) s_link = LINK_UNAUTHORIZED;
  else if (strstr(s_view.error, "unsupported") || strstr(s_view.error, "bridge_unsupported")) s_link = LINK_UNSUPPORTED;
  else if (strstr(s_view.error, "offline")) s_link = LINK_OFFLINE;

  if ((is_terminal_send_ack || (s_view.disposition[0] && (!strcmp(s_view.disposition, "accepted") || !strcmp(s_view.disposition, "queued")))) &&
      (s_active_command == AGENT_CMD_SEND || s_active_command == AGENT_CMD_START_NEW)) {
    reset_draft();
    s_sent_from_watch = true;
    s_completion_alert_pending = true;
    s_send_unconfirmed = false;
    if (s_view.session_id[0]) s_screen = SCREEN_CHAT;
  }
  if (s_screen == SCREEN_RUNTIMES && !strcmp(s_view.status, "agents")) s_screen = SCREEN_AGENTS;
  else if (s_screen == SCREEN_AGENTS && !strcmp(s_view.status, "sessions")) s_screen = SCREEN_SESSIONS;
  else if ((s_screen == SCREEN_SESSIONS || s_screen == SCREEN_CHAT) && !strcmp(s_view.status, "chat")) s_screen = SCREEN_CHAT;
  else if (!strcmp(s_view.status, "runtimes")) s_screen = SCREEN_RUNTIMES;
  else if (!strcmp(s_view.status, "agents")) s_screen = SCREEN_AGENTS;
  else if (!strcmp(s_view.status, "sessions")) s_screen = SCREEN_SESSIONS;
  else if (!strcmp(s_view.status, "error")) {
    if (strstr(s_view.error, "unauthorized")) s_link = LINK_UNAUTHORIZED;
    else if (strstr(s_view.error, "unsupported") || strstr(s_view.error, "bridge_unsupported")) s_link = LINK_UNSUPPORTED;
    else s_link = LINK_OFFLINE;
    copy_exact(s_voice_error, sizeof(s_voice_error), s_view.error[0] ? s_view.error : "Phone bridge returned an error");
  }

  if (s_screen == SCREEN_CHAT) {
    if (entering_chat || was_at_tail || !old_count) s_selected = s_row_count;
    else {
      bool preserved = false;
      for (uint8_t i = 0; i < s_row_count; i++) if (selected_id[0] && !strcmp(selected_id, s_rows[i].id)) {
        s_selected = i; preserved = true; break;
      }
      if (!preserved && s_selected >= s_row_count) s_selected = s_row_count ? (uint8_t)(s_row_count - 1) : 0;
    }
    if (!was_older && old_newest[0] && !was_at_tail && !keep_reading_page) {
      for (uint8_t i = 0; i < s_row_count; i++) if (!strcmp(old_newest, s_rows[i].id)) {
        s_new_count = (uint8_t)(s_row_count - i - 1); break;
      }
    }
    if (s_attention_known && (!old_known || s_first_baseline)) {
      s_old_attention_count = (uint32_t)s_view.approval_count + s_view.elicitation_count;
      s_attention_baseline = true;
    } else if (s_attention_known && s_attention_baseline) {
      uint32_t current = (uint32_t)s_view.approval_count + s_view.elicitation_count;
      if (current > old_attention && !s_first_baseline) alert_user(true);
      s_old_attention_count = current;
    }
    if (!was_at_tail && s_row_count > old_count && !keep_reading_page && !was_older) s_new_count = s_row_count - old_count;
    else if (was_at_tail) { s_new_count = 0; s_new_row_count = 0; }
    bool attention_increased = s_attention_known && s_attention_baseline &&
      (uint32_t)s_view.approval_count + s_view.elicitation_count > old_attention;
    if (s_view.alert && !s_first_baseline) {
      if (s_view.alert == 1) alert_user(true);
      else if (!attention_increased) alert_user(false);
    } else if (old_running && !s_running && !s_first_baseline && !attention_increased && s_completion_alert_pending)
      alert_user(false);
    if ((old_running && !s_running && s_completion_alert_pending) || s_view.alert == 2) s_completion_alert_pending = false;
    s_sent_from_watch = false;
    s_first_baseline = false;
  } else s_selected = 0;

  if ((!strcmp(s_view.status, "accepted") || !strcmp(s_view.status, "queued")) &&
      (s_active_command == AGENT_CMD_SEND || s_active_command == AGENT_CMD_START_NEW)) {
    s_send_unconfirmed = false;
    if (!s_view.session_id[0] && s_view.disposition[0]) s_send_unconfirmed = false;
    if (s_view.session_id[0] && s_screen == SCREEN_CHAT) schedule_poll(350);
  }
  clear_stage();
  render();
  schedule_poll(s_screen == SCREEN_CHAT ? (s_running ? 5000 : 15000) : 30000);
}

static void fail_staged_view(const char *reason) {
  clear_stage();
  s_loading_older = false;
  s_link = LINK_UNSUPPORTED;
  copy_exact(s_voice_error, sizeof(s_voice_error), reason);
  render();
}

static void frame_received(DictionaryIterator *iter) {
  uint32_t request = 0, frame = 0, index = UINT32_MAX, total = 0;
  if (!tuple_uint(iter, MESSAGE_KEY_request_id, &request, true) || request != s_active_request) return;
  if (!tuple_uint(iter, MESSAGE_KEY_frame, &frame, true)) return;
  s_sending_ack = false;
  if (frame == AGENT_FRAME_ERROR) {
    s_voice_error[0] = '\0';
    if (!tuple_string(iter, MESSAGE_KEY_error, s_voice_error, sizeof(s_voice_error), true))
      copy_exact(s_voice_error, sizeof(s_voice_error), "Phone bridge error · retry");
    bool is_send_error = s_send_unconfirmed;
    s_busy = false; stop_request_timer(); clear_stage(); s_loading_older = false;
    if (is_send_error) s_send_unconfirmed = true;
    s_error_return_screen = s_screen; s_screen = SCREEN_ERROR;
    if (strstr(s_voice_error, "unauthorized") || strstr(s_voice_error, "needs_auth")) s_link = LINK_UNAUTHORIZED;
    else if (strstr(s_voice_error, "unsupported") || strstr(s_voice_error, "bridge_unsupported")) s_link = LINK_UNSUPPORTED;
    else s_link = LINK_OFFLINE;
    render();
    s_poll_failures++;
    if (s_poll_failures < 3 && s_polling_enabled) schedule_poll(s_poll_failures == 1 ? 2000 : 5000);
    return;
  }
  if (frame == AGENT_FRAME_BEGIN) index = 0;
  else if (!tuple_uint(iter, MESSAGE_KEY_index, &index, true)) return;
  if (!tuple_uint(iter, MESSAGE_KEY_total, &total, true) || total < 3 || total > MAX_FRAME_ROWS + 3) return;
  if (s_have_last_frame && index == s_last_frame_index && frame == s_last_frame_kind) {
    send_ack(request, frame, index);
    return;
  }
  if (s_have_last_frame && (index < s_last_frame_index || index > s_last_frame_index + 1)) return;
  if (frame == AGENT_FRAME_BEGIN) {
    if (index != 0 || s_stage_active) return;
    s_frame_total = total;
    s_next_frame = 1;
    s_stage_active = true;
    uint32_t response_type = 0;
    if (!tuple_uint(iter, MESSAGE_KEY_type, &response_type, true) || response_type != AGENT_CMD_SNAPSHOT ||
        !tuple_string(iter, MESSAGE_KEY_status, s_staged_view.status, sizeof(s_staged_view.status), true)) {
      fail_staged_view("Invalid begin frame"); return;
    }
    if (!strcmp(s_staged_view.status, "runtimes")) { s_frame_row_limit = 8; s_frame_row_kind = AGENT_ROW_RUNTIME; }
    else if (!strcmp(s_staged_view.status, "agents")) { s_frame_row_limit = 4; s_frame_row_kind = AGENT_ROW_AGENT; }
    else if (!strcmp(s_staged_view.status, "sessions")) { s_frame_row_limit = 5; s_frame_row_kind = AGENT_ROW_SESSION; }
    else if (!strcmp(s_staged_view.status, "chat")) { s_frame_row_limit = 5; s_frame_row_kind = AGENT_ROW_TURN; }
    else if (!strcmp(s_staged_view.status, "sending") || !strcmp(s_staged_view.status, "accepted") ||
             !strcmp(s_staged_view.status, "queued") || !strcmp(s_staged_view.status, "error")) {
      s_frame_row_limit = 0; s_frame_row_kind = UINT8_MAX;
    } else { fail_staged_view("Unknown snapshot view"); return; }
    if (total > (uint32_t)s_frame_row_limit + 3) { fail_staged_view("Snapshot row count exceeds the view budget"); return; }
  } else {
    if (!s_stage_active || total != s_frame_total || index != s_next_frame) return;
    if (frame == AGENT_FRAME_META) {
      if (index != 1 || !parse_meta(iter, &s_staged_view)) { fail_staged_view("Invalid metadata frame"); return; }
    } else if (frame == AGENT_FRAME_ROW) {
      uint32_t row_count = total - 3;
      ViewRow parsed;
      if (index < 2 || index > row_count + 1 || s_staged_count >= MAX_FRAME_ROWS ||
          !parse_row(iter, &parsed, index - 2) || parsed.kind != s_frame_row_kind) {
        fail_staged_view("Invalid or oversized row · view not updated"); return;
      }
      s_staged_rows[s_staged_count++] = parsed;
    } else if (frame == AGENT_FRAME_END) {
      if (index != total - 1 || s_staged_count != total - 3) { fail_staged_view("Incomplete snapshot · view not updated"); return; }
      s_last_frame_kind = frame; s_last_frame_index = index; s_have_last_frame = true;
      send_ack(request, frame, index);
      stop_request_timer(); s_busy = false;
      publish_view();
      return;
    } else return;
    s_next_frame++;
  }
  s_last_frame_kind = frame; s_last_frame_index = index; s_have_last_frame = true;
  send_ack(request, frame, index);
}

static void inbox_received(DictionaryIterator *iter, void *context) {
  (void)context;
  if (s_busy) { frame_received(iter); return; }
  uint32_t request = 0, frame = 0, index = UINT32_MAX;
  if (s_have_last_frame && tuple_uint(iter, MESSAGE_KEY_request_id, &request, true) && request == s_active_request &&
      tuple_uint(iter, MESSAGE_KEY_frame, &frame, true) && tuple_uint(iter, MESSAGE_KEY_index, &index, true) &&
      index == s_last_frame_index && frame == s_last_frame_kind) send_ack(request, frame, index);
}

static void outbox_failed(DictionaryIterator *iter, AppMessageResult reason, void *context) {
  (void)iter; (void)context;
  if (s_sending_ack) { s_sending_ack = false; return; }
  if (!s_busy) return;
  s_busy = false; stop_request_timer(); clear_stage(); s_loading_older = false;
  s_link = reason == APP_MSG_NOT_CONNECTED ? LINK_OFFLINE : LINK_BUSY;
  s_send_unconfirmed = s_active_command == AGENT_CMD_SEND || s_active_command == AGENT_CMD_START_NEW;
  s_error_return_screen = s_screen; s_screen = SCREEN_ERROR;
  copy_exact(s_voice_error, sizeof(s_voice_error), s_send_unconfirmed ? "Not confirmed · retry uses the same draft key" : "Phone response failed · retry refresh");
  render();
}

static void select_action(void) {
  if (s_busy) return;
  switch (s_screen) {
    case SCREEN_RUNTIMES:
      if (s_selected < s_row_count) {
        ViewRow *row = &s_rows[s_selected];
        send_command(AGENT_CMD_SELECT, row->id, NULL, NULL, NULL, NULL, NULL, NULL);
      }
      break;
    case SCREEN_AGENTS:
      if (s_selected < s_row_count) {
        ViewRow *row = &s_rows[s_selected];
        send_command(AGENT_CMD_SELECT, NULL, row->id, row->meta, NULL, NULL, NULL, NULL);
      }
      break;
    case SCREEN_SESSIONS:
      if (s_selected == 0) {
        s_view.session_id[0] = '\0'; s_view.session_title[0] = '\0';
        s_row_count = 0; s_staged_count = 0;
        s_screen = SCREEN_CHAT; s_selected = 0; s_execution_available = true; mark_user_activity(); render();
      } else if (s_selected <= s_row_count) {
        ViewRow *row = &s_rows[s_selected - 1];
        s_view.session_id[0] = '\0';
        send_command(AGENT_CMD_OPEN_SESSION, NULL, NULL, NULL, row->id, NULL, NULL, NULL);
      }
      break;
    case SCREEN_CHAT:
      if (s_selected < s_row_count) {
        if (s_rows[s_selected].kind == AGENT_ROW_TURN) {
          copy_exact(s_reader, sizeof(s_reader), s_rows[s_selected].text);
          if (s_rows[s_selected].truncated && strlen(s_reader) + 26 < sizeof(s_reader)) strcat(s_reader, "\n\nContinues on phone.");
          s_reader_offset = 0; s_screen = SCREEN_READER; s_reader_chat_return = true; render();
        }
      } else if (s_draft[0]) { s_screen = SCREEN_REVIEW; render(); }
      else if (s_execution_available) open_compose_menu();
      break;
    case SCREEN_READER: break;
    case SCREEN_REVIEW: open_review_menu(); break;
    case SCREEN_MENU: menu_action_selected(); break;
    case SCREEN_STATUS: s_screen = SCREEN_CHAT; render(); break;
    case SCREEN_ERROR:
      if (s_send_unconfirmed && s_draft[0]) { s_screen = SCREEN_REVIEW; s_send_unconfirmed = true; open_review_menu(); }
      else {
        if (strstr(s_voice_error, "Older history expired")) { s_view.before_cursor[0] = '\0'; s_view.truncated_before = false; }
        s_screen = s_error_return_screen; s_poll_failures = 0; mark_user_activity(); send_poll();
      }
      break;
    case SCREEN_VOICE_ERROR: open_voice_menu(); break;
  }
}

static void jump_to_newest(void) {
  if (s_new_row_count) {
    s_row_count = s_new_row_count;
    memcpy(s_rows, s_new_rows, s_row_count * sizeof(ViewRow));
    copy_exact(s_view.before_cursor, sizeof(s_view.before_cursor), s_new_cursor);
    s_view.truncated_before = s_new_cursor[0] != '\0';
    s_new_row_count = 0;
  }
  s_selected = s_row_count;
  s_new_count = 0;
  render();
}

static void move_up(void) {
  if (s_screen == SCREEN_READER) {
    if (s_reader_offset > 48) s_reader_offset -= 48; else s_reader_offset = 0;
  } else if (s_screen == SCREEN_CHAT) {
    if (s_selected < s_row_count && s_selected > 0) s_selected--;
    else if (s_selected >= s_row_count && s_row_count) s_selected = s_row_count - 1;
    else if (!s_selected && s_view.truncated_before && s_view.before_cursor[0])
      if (!s_busy) { s_loading_older = true; send_command(AGENT_CMD_OLDER, NULL, NULL, NULL, s_view.session_id, s_view.before_cursor, NULL, NULL); }
  } else if (s_screen == SCREEN_MENU) { if (s_menu_selected) s_menu_selected--; }
  else if (s_selected) s_selected--;
  render();
}

static void move_down(void) {
  if (s_screen == SCREEN_READER) {
    size_t length = strlen(s_reader);
    if ((size_t)s_reader_offset + 48 < length) s_reader_offset += 48;
  } else if (s_screen == SCREEN_CHAT) {
    if (s_new_count) jump_to_newest();
    else if (s_selected < s_row_count) s_selected++;
  } else if (s_screen == SCREEN_MENU) {
    if (s_menu_selected + 1 < s_menu_count) s_menu_selected++;
  } else {
    uint8_t max = s_screen == SCREEN_SESSIONS ? s_row_count : s_row_count ? (uint8_t)(s_row_count - 1) : 0;
    if (s_selected < max) s_selected++;
  }
  render();
}

static void go_back(void) {
  switch (s_screen) {
    case SCREEN_AGENTS: s_screen = SCREEN_RUNTIMES; break;
    case SCREEN_SESSIONS: s_screen = SCREEN_AGENTS; break;
    case SCREEN_CHAT:
      if (s_view.agent_name[0]) {
        s_screen = SCREEN_SESSIONS; s_row_count = 0; s_selected = 0;
        send_command(AGENT_CMD_SELECT, NULL, s_view.agent_name, s_view.source, NULL, NULL, NULL, NULL);
      } else s_screen = SCREEN_AGENTS;
      break;
    case SCREEN_READER: s_screen = SCREEN_CHAT; break;
    case SCREEN_REVIEW: s_screen = s_view.session_id[0] ? SCREEN_CHAT : SCREEN_SESSIONS; break;
    case SCREEN_STATUS: s_screen = SCREEN_CHAT; break;
    case SCREEN_ERROR: s_screen = s_error_return_screen; break;
    case SCREEN_VOICE_ERROR: s_screen = s_view.session_id[0] ? SCREEN_CHAT : SCREEN_SESSIONS; break;
    case SCREEN_MENU: s_screen = s_menu_kind == MENU_REVIEW ? SCREEN_REVIEW :
      s_menu_kind == MENU_VOICE_ERROR ? SCREEN_VOICE_ERROR :
      s_menu_kind == MENU_CANNED && s_draft[0] ? SCREEN_REVIEW : (s_view.session_id[0] ? SCREEN_CHAT : SCREEN_SESSIONS); break;
    case SCREEN_RUNTIMES: break;
  }
  render();
}

static void mark_user_activity(void) {
  uint32_t now = monotonic_ms(); s_last_activity_ms = now; s_poll_window_started = now;
  s_poll_failures = 0; s_polling_enabled = true;
  if (s_screen == SCREEN_CHAT) schedule_poll(s_running ? 5000 : 15000);
  else if (s_screen == SCREEN_RUNTIMES || s_screen == SCREEN_AGENTS || s_screen == SCREEN_SESSIONS) schedule_poll(30000);
}
static void click_up(ClickRecognizerRef ref, void *ctx) { (void)ref; (void)ctx; mark_user_activity(); move_up(); }
static void click_down(ClickRecognizerRef ref, void *ctx) { (void)ref; (void)ctx; mark_user_activity(); move_down(); }
static void click_select(ClickRecognizerRef ref, void *ctx) { (void)ref; (void)ctx; mark_user_activity(); select_action(); }
static void click_select_long(ClickRecognizerRef ref, void *ctx) { (void)ref; (void)ctx; mark_user_activity(); if (s_screen == SCREEN_CHAT) start_dictation(); }
static void click_down_long(ClickRecognizerRef ref, void *ctx) { (void)ref; (void)ctx; mark_user_activity(); if (s_screen == SCREEN_CHAT) { s_screen = SCREEN_STATUS; render(); } }
static void click_back(ClickRecognizerRef ref, void *ctx) { (void)ref; (void)ctx; mark_user_activity(); go_back(); }
static void click_config(void *ctx) {
  (void)ctx;
  window_single_click_subscribe(BUTTON_ID_UP, click_up);
  window_single_click_subscribe(BUTTON_ID_DOWN, click_down);
  window_single_click_subscribe(BUTTON_ID_SELECT, click_select);
  window_long_click_subscribe(BUTTON_ID_SELECT, 500, click_select_long, NULL);
  window_long_click_subscribe(BUTTON_ID_DOWN, 500, click_down_long, NULL);
  window_single_click_subscribe(BUTTON_ID_BACK, click_back);
}

static void touch_handler(const TouchEvent *event, void *context) {
  (void)context;
  if (!touch_service_is_enabled() || event->non_navigational) return;
  if (event->type == TouchEvent_Touchdown) {
    s_touch_active = true; s_touch_moved = false;
    s_touch_x = event->x; s_touch_y = event->y; s_touch_last_y = event->y;
    s_touch_down_at = monotonic_ms();
  } else if (event->type == TouchEvent_PositionUpdate && s_touch_active) {
    int16_t delta = (int16_t)(event->y - s_touch_last_y);
    if (delta > 12 || delta < -12) s_touch_moved = true;
    if (s_touch_moved) {
      if (s_screen == SCREEN_READER) {
        if (delta < -10) move_down(); else if (delta > 10) move_up();
      } else if ((s_screen == SCREEN_CHAT || s_screen == SCREEN_RUNTIMES ||
                  s_screen == SCREEN_AGENTS || s_screen == SCREEN_SESSIONS) &&
                 (delta < -30 || delta > 30)) {
        if (delta < 0) move_down(); else move_up();
        s_touch_last_y = event->y;
      }
    }
    s_touch_last_y = event->y;
  } else if (event->type == TouchEvent_Liftoff && s_touch_active) {
    s_touch_active = false;
    if (s_touch_moved || (monotonic_ms() - s_last_layout_at < 300)) return;
    mark_user_activity();
    if (s_touch_y < 30) { s_screen = SCREEN_STATUS; render(); return; }
    if (s_screen == SCREEN_REVIEW && s_touch_y > 175) { open_review_menu(); return; }
    if (s_screen == SCREEN_MENU) {
      uint8_t row = s_touch_y < 55 ? 0 : (uint8_t)((s_touch_y - 55) / 38);
      if (row < s_menu_count) s_menu_selected = row;
      render(); return;
    }
    if (s_screen == SCREEN_CHAT && s_touch_y > 185 && s_new_count) { jump_to_newest(); return; }
    uint8_t row = s_touch_y < 53 ? 0 : (uint8_t)((s_touch_y - 53) / 42);
    uint8_t visible_base = s_selected > 2 ? (uint8_t)(s_selected - 2) : 0;
    if (s_screen == SCREEN_CHAT) {
      uint8_t chat_base = s_selected > 1 ? (uint8_t)(s_selected - 1) : 0;
      uint8_t chosen = chat_base + row;
      s_selected = chosen < s_row_count ? chosen : s_row_count;
    } else if (s_screen == SCREEN_SESSIONS) {
      uint8_t chosen = visible_base + row;
      s_selected = chosen > s_row_count ? s_row_count : chosen;
    } else if (visible_base + row < s_row_count) {
      s_selected = visible_base + row;
    }
    render();
  }
}

static void connection_changed(bool connected) {
  if (connected) { s_link = LINK_CONNECTING; s_poll_failures = 0; s_polling_enabled = true; mark_user_activity(); send_poll(); }
  else { s_link = LINK_OFFLINE; if (s_poll_timer) app_timer_cancel(s_poll_timer); s_poll_timer = NULL; render(); }
}

static void focus_changed(bool focused) {
  s_focused = focused;
  if (!focused) {
    if (s_poll_timer) app_timer_cancel(s_poll_timer);
    s_poll_timer = NULL;
    stop_request_timer(); s_busy = false; clear_stage();
    return;
  }
  s_first_baseline = true; s_attention_baseline = false; s_poll_failures = 0; s_polling_enabled = true;
  s_last_activity_ms = monotonic_ms(); s_poll_window_started = s_last_activity_ms; send_poll();
}

static void window_load(Window *window) {
  Layer *root = window_get_root_layer(window);
  GRect bounds = layer_get_bounds(root);
  s_band = layer_create(GRect(0, 0, bounds.size.w, 25));
  layer_set_update_proc(s_band, band_draw); layer_add_child(root, s_band);
  s_title = text_layer_create(GRect(7, 27, bounds.size.w - 14, 24));
  text_layer_set_font(s_title, fonts_get_system_font(FONT_KEY_GOTHIC_18_BOLD));
  text_layer_set_text_color(s_title, GColorBlack); text_layer_set_background_color(s_title, GColorClear);
  layer_add_child(root, text_layer_get_layer(s_title));
  s_body = text_layer_create(GRect(8, 53, bounds.size.w - 16, bounds.size.h - 82));
  text_layer_set_font(s_body, fonts_get_system_font(FONT_KEY_GOTHIC_18));
  text_layer_set_text_color(s_body, GColorBlack); text_layer_set_background_color(s_body, GColorWhite);
  text_layer_set_overflow_mode(s_body, GTextOverflowModeTrailingEllipsis);
  layer_add_child(root, text_layer_get_layer(s_body));
  s_footer = text_layer_create(GRect(8, bounds.size.h - 25, bounds.size.w - 16, 21));
  text_layer_set_font(s_footer, fonts_get_system_font(FONT_KEY_GOTHIC_14));
  text_layer_set_text_color(s_footer, GColorDarkGray); text_layer_set_background_color(s_footer, GColorWhite);
  layer_add_child(root, text_layer_get_layer(s_footer));
  render();
}

static void window_unload(Window *window) {
  (void)window;
  text_layer_destroy(s_footer); text_layer_destroy(s_body); text_layer_destroy(s_title); layer_destroy(s_band);
  s_footer = s_body = s_title = NULL; s_band = NULL;
}

static void init(void) {
  srand((unsigned)time(NULL));
  s_window = window_create(); window_set_background_color(s_window, GColorWhite);
  window_set_window_handlers(s_window, (WindowHandlers){ .load = window_load, .unload = window_unload });
  window_set_click_config_provider(s_window, click_config); window_stack_push(s_window, true);
  app_focus_service_subscribe_handlers((AppFocusHandlers){ .did_focus = focus_changed });
  connection_service_subscribe((ConnectionHandlers){ .pebble_app_connection_handler = connection_changed });
  app_message_register_inbox_received(inbox_received);
  app_message_register_outbox_failed(outbox_failed);
  uint32_t inbox_max = app_message_inbox_size_maximum();
  uint32_t outbox_max = app_message_outbox_size_maximum();
  s_inbox_capacity = inbox_max < 4096 ? inbox_max : 4096;
  s_outbox_capacity = outbox_max < APP_OUTBOX_TARGET ? outbox_max : APP_OUTBOX_TARGET;
  app_message_open(s_inbox_capacity, s_outbox_capacity);
  if (s_inbox_capacity < SUPPORTED_INBOX_MIN || s_outbox_capacity < 512) {
    s_link = LINK_UNSUPPORTED;
    copy_exact(s_voice_error, sizeof(s_voice_error), "This phone bridge needs a 1024-byte inbox and 512-byte outbox.");
    render(); return;
  }
  app_touch_navigation_enable(false);
  touch_service_subscribe(touch_handler, NULL);
  send_poll();
}

static void deinit(void) {
  if (s_poll_timer) app_timer_cancel(s_poll_timer);
  stop_request_timer();
  if (s_dictation) dictation_session_destroy(s_dictation);
  touch_service_unsubscribe(); app_touch_navigation_enable(false);
  connection_service_unsubscribe(); app_focus_service_unsubscribe(); app_message_deregister_callbacks(); window_destroy(s_window);
}

int main(void) { init(); app_event_loop(); deinit(); }
