-- Expand-only. Compatible with the pre-blue/green release, which ignores these tables.
CREATE TABLE IF NOT EXISTS deployment_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS legacy_rag_documents (
    id BIGSERIAL PRIMARY KEY,
    session_id TEXT NOT NULL,
    speaker TEXT NOT NULL DEFAULT '',
    start_time DOUBLE PRECISION NOT NULL DEFAULT 0,
    end_time DOUBLE PRECISION NOT NULL DEFAULT 0,
    original_text TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    hash TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS legacy_rag_session_time ON legacy_rag_documents(session_id, start_time);
CREATE TABLE IF NOT EXISTS legacy_rag_embeddings (
    doc_id BIGINT PRIMARY KEY REFERENCES legacy_rag_documents(id) ON DELETE CASCADE,
    dim INTEGER NOT NULL,
    norm DOUBLE PRECISION NOT NULL,
    vector_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS legacy_rag_session_summary (
    session_id TEXT PRIMARY KEY,
    summary TEXT NOT NULL DEFAULT '',
    title TEXT,
    updated_at TIMESTAMPTZ NOT NULL
);
