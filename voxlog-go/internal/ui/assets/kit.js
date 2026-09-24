// The handful of helpers more than one page needs. Spliced into the main
// window and into the drawer, the same way kit.css is -- the drawer used to
// carry its own copy of every one of these, and they had drifted: a project
// drew in a different colour in each window, and in_progress was called
// "In progress" in one and "Doing" in the other.

// Escaping goes through the DOM rather than a regexp: the two hand-written
// regexps this replaces escaped different sets of characters, and one of them
// left the apostrophe alone.
function escapeHtml(s) {
  var d = document.createElement('div');
  d.textContent = s == null ? '' : s;
  return d.innerHTML;
}

// entityColor turns a project name into a stable colour, so the same project
// always draws the same dot without a colour picked or stored anywhere -- a
// plain string hash into a small fixed palette.
function entityColor(name) {
  name = name || '';
  // Unfiltered is the catch-all, not a real project -- a neutral dot says so
  // at a glance instead of it looking like just another configured project.
  if (name === 'Unfiltered') return '#8e8e93';
  var hash = 0;
  for (var i = 0; i < name.length; i++) { hash = (hash * 31 + name.charCodeAt(i)) >>> 0; }
  var hues = ['#007aff', '#af52de', '#34c759', '#ff9500', '#ff3b30', '#5e5ce6', '#00c7be'];
  return hues[hash % hues.length];
}

// One name per status, everywhere it is shown.
var taskStatusLabel = { todo: 'To do', in_progress: 'In progress', blocked: 'Blocked', done: 'Done' };

// ...and one spelling of it in CSS. in_progress is the stored value; the
// class is the same string, so a rule can be written for either without two
// of them meaning the same state.
function taskStatusClass(status) { return 'status-' + status; }

// Recording length, as hours/minutes/seconds -- a call is not a number of
// seconds anybody reads.
function duration(seconds) {
  var total = Math.round(seconds || 0);
  var h = Math.floor(total / 3600), m = Math.floor(total / 60) % 60, s = total % 60;
  if (h > 0) return h + 'h ' + m + 'm';
  if (m > 0) return m + 'm ' + (s < 10 ? '0' : '') + s + 's';
  return s + 's';
}

// MM:SS, for a position inside a recording and for elapsed time in the live
// banner. Both used to have their own formatter, differing only in whether
// the minutes carried a leading zero.
function clockTime(seconds) {
  var total = Math.max(0, Math.round(seconds || 0));
  var m = Math.floor(total / 60), s = total % 60;
  return (m < 10 ? '0' : '') + m + ':' + (s < 10 ? '0' : '') + s;
}

// toast says what just happened without blocking the window. alert() cannot
// do that job here at all: webview_go installs no WKUIDelegate, so alert,
// confirm and prompt are no-ops -- confirm answers false and prompt answers
// null, which is exactly why the Voices pane's Rename, Forget and Erase
// buttons looked dead.
var toastTimer = null;
function toast(message) {
  var el = document.getElementById('toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'toast';
    el.className = 'toast';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    document.body.appendChild(el);
  }
  el.textContent = message;
  el.classList.add('on');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(function () { el.classList.remove('on'); }, 2600);
}
