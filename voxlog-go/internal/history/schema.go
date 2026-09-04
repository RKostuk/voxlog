package history

import "fmt"

// schemaSteps is the schema as an ordered list of migrations, applied by
// PRAGMA user_version. Append to it, never edit a step that has shipped: an
// existing database only runs the steps past its own version.
var schemaSteps = []string{
	// Step 1 -- meetings, their speakers, their turns, and the voices those
	// speakers are recognised as.
	`
	CREATE TABLE meetings (
		-- Start.UnixNano(). Every caller already identifies a meeting by its
		-- start instant (meeting.go looks one up by Start, the window sends
		-- it as an RFC3339Nano string), so that is the key rather than an
		-- invented id nothing outside this table would know.
		start_ns          INTEGER PRIMARY KEY,
		recording_secs    REAL    NOT NULL DEFAULT 0,
		-- decode_secs is how long transcription took, not how long the call
		-- was; the window shows both, and they are wildly different numbers.
		decode_secs       REAL    NOT NULL DEFAULT 0,
		text              TEXT    NOT NULL DEFAULT '',
		summary           TEXT    NOT NULL DEFAULT '',
		audio_path        TEXT    NOT NULL DEFAULT '',
		system_audio_path TEXT    NOT NULL DEFAULT '',
		-- entity is the project this meeting belongs to, set by hand. Until
		-- now a meeting only had a project if Task Hub happened to find a
		-- task in it, which left most meetings unfilterable.
		entity            TEXT    NOT NULL DEFAULT '',
		-- turns_version is the extraction version behind the rows in turns:
		-- 0 means none were ever extracted. Bumping the constant in the app
		-- is what re-runs every meeting through an improved pipeline.
		turns_version     INTEGER NOT NULL DEFAULT 0,
		backfill_attempts INTEGER NOT NULL DEFAULT 0,
		backfill_error    TEXT    NOT NULL DEFAULT ''
	);

	CREATE TABLE voices (
		id         INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL,
		created_ns INTEGER NOT NULL,
		updated_ns INTEGER NOT NULL,
		-- embed is the L2-normalised centroid of every sample behind this
		-- voice, little-endian float32. secs is how much speech went into it,
		-- which is the weight when a new meeting folds into the centroid.
		-- NULL until the first speaker is linked: a voice is created and
		-- given its fingerprint in one transaction, and the name comes first.
		embed      BLOB,
		secs       REAL    NOT NULL DEFAULT 0,
		-- clip is a couple of seconds of this voice as a small WAV, so the
		-- Voices pane can still play a sample after retention has swept the
		-- recording it came from.
		clip       BLOB,
		-- name_key is the name folded to lower case by Go, and it is what
		-- uniqueness is enforced on. SQLite's own NOCASE collation only folds
		-- ASCII, so it would happily keep "Олег" and "олег" as two different
		-- people -- which is every name this app's user actually types.
		name_key   TEXT    NOT NULL
	);
	CREATE UNIQUE INDEX voices_name_key ON voices(name_key);

	CREATE TABLE meeting_speakers (
		id         INTEGER PRIMARY KEY,
		meeting_ns INTEGER NOT NULL REFERENCES meetings(start_ns) ON DELETE CASCADE,
		-- local_id numbers the speakers of this meeting in the order they
		-- first talk; -1 is the microphone, i.e. the user.
		local_id   INTEGER NOT NULL,
		-- SET NULL rather than CASCADE: forgetting a voice must un-name its
		-- turns, never delete them.
		voice_id   INTEGER REFERENCES voices(id) ON DELETE SET NULL,
		-- talk_secs and turn_count are denormalised on purpose. The meetings
		-- list draws a talk-time bar per row, and that must not become a scan
		-- over every turn of every meeting.
		talk_secs  REAL    NOT NULL DEFAULT 0,
		turn_count INTEGER NOT NULL DEFAULT 0,
		embed      BLOB,
		UNIQUE(meeting_ns, local_id)
	);
	CREATE INDEX meeting_speakers_unnamed ON meeting_speakers(voice_id);

	CREATE TABLE turns (
		id         INTEGER PRIMARY KEY,
		meeting_ns INTEGER NOT NULL REFERENCES meetings(start_ns) ON DELETE CASCADE,
		seq        INTEGER NOT NULL,
		-- 0 microphone, 1 system audio. Which file a reply is played from.
		channel    INTEGER NOT NULL DEFAULT 0,
		-- Seconds from the start of the recording, not from the start of the
		-- 60-second block this turn was decoded in. That distinction is the
		-- whole reason per-reply playback is possible.
		start_secs REAL    NOT NULL,
		end_secs   REAL    NOT NULL,
		speaker_id INTEGER REFERENCES meeting_speakers(id) ON DELETE SET NULL,
		text       TEXT    NOT NULL,
		UNIQUE(meeting_ns, seq)
	);
	CREATE INDEX turns_by_meeting ON turns(meeting_ns, start_secs);
	`,
	// Step 2 -- full-text search across every meeting's turns.
	//
	// An external-content FTS5 table: the text lives in turns and is not
	// copied here, so the index cannot disagree with the rows it indexes and
	// the database does not carry two copies of every transcript. The
	// triggers are the standard external-content set -- FTS5 needs the OLD
	// text on delete and update, which is what the 'delete' command rows do.
	`
	CREATE VIRTUAL TABLE turns_fts USING fts5(
		text,
		content='turns',
		content_rowid='id',
		tokenize='unicode61 remove_diacritics 2'
	);

	CREATE TRIGGER turns_fts_insert AFTER INSERT ON turns BEGIN
		INSERT INTO turns_fts(rowid, text) VALUES (new.id, new.text);
	END;
	CREATE TRIGGER turns_fts_delete AFTER DELETE ON turns BEGIN
		INSERT INTO turns_fts(turns_fts, rowid, text) VALUES ('delete', old.id, old.text);
	END;
	CREATE TRIGGER turns_fts_update AFTER UPDATE ON turns BEGIN
		INSERT INTO turns_fts(turns_fts, rowid, text) VALUES ('delete', old.id, old.text);
		INSERT INTO turns_fts(rowid, text) VALUES (new.id, new.text);
	END;

	-- Meetings recorded before this step already have their turns: index
	-- them once here rather than making every caller wonder whether search
	-- covers the whole history or only what has been recorded since.
	INSERT INTO turns_fts(rowid, text) SELECT id, text FROM turns;
	`,
	// Step 3 -- a meeting nobody asked for.
	//
	// Always-on listening starts a recording on its own when it hears
	// speech. Those have to be distinguishable from the ones the user
	// deliberately started, because retention treats them differently: an
	// auto-started recording that never turned into a transcript is swept
	// after a day, and one the user pressed a key for never is.
	`
	ALTER TABLE meetings ADD COLUMN auto_started INTEGER NOT NULL DEFAULT 0;
	`,
}

func (d *DB) migrateSchema() error {
	var version int
	if err := d.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("history: reading schema version: %w", err)
	}
	if version >= len(schemaSteps) {
		return nil
	}

	for i := version; i < len(schemaSteps); i++ {
		tx, err := d.sql.Begin()
		if err != nil {
			return fmt.Errorf("history: schema step %d: %w", i+1, err)
		}
		if _, err := tx.Exec(schemaSteps[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("history: schema step %d: %w", i+1, err)
		}
		// PRAGMA user_version takes no bound parameters, hence the format --
		// the value is a loop counter, never user input.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("history: schema step %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("history: schema step %d: %w", i+1, err)
		}
	}
	return nil
}
