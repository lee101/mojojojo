package main

import "database/sql"

// migrateDatabase owns the complete mojojojo data model. Nothing here is a
// foreign app.nz table or a path to another service's database.
func migrateDatabase(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL,
			handle TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL,
			free_credits BIGINT NOT NULL DEFAULT 0 CHECK (free_credits >= 0),
			plan_credits BIGINT NOT NULL DEFAULT 0 CHECK (plan_credits >= 0),
			plan_credits_expire_at TIMESTAMPTZ,
			paid_credits BIGINT NOT NULL DEFAULT 0 CHECK (paid_credits >= 0),
			is_admin BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			disabled_at TIMESTAMPTZ
		);
		CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_uq ON users (LOWER(email));

		CREATE TABLE IF NOT EXISTS email_preferences (
			user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			product_updates BOOLEAN NOT NULL DEFAULT FALSE,
			opted_in_at TIMESTAMPTZ,
			unsubscribed_at TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS email_campaign_enrollments (
			campaign_id TEXT NOT NULL,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			enrolled_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (campaign_id, user_id)
		);
		CREATE TABLE IF NOT EXISTS email_campaign_sends (
			campaign_id TEXT NOT NULL,
			email_id TEXT NOT NULL,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			status TEXT NOT NULL CHECK (status IN ('sending', 'sent', 'failed')),
			attempt_count INTEGER NOT NULL DEFAULT 1 CHECK (attempt_count > 0),
			attempted_at TIMESTAMPTZ NOT NULL,
			sent_at TIMESTAMPTZ,
			message_id TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (campaign_id, email_id, user_id)
		);
		CREATE INDEX IF NOT EXISTS email_campaign_sends_user_idx
			ON email_campaign_sends(user_id, attempted_at DESC);

		CREATE TABLE IF NOT EXISTS sessions (
			id_hash TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			last_seen_at TIMESTAMPTZ NOT NULL,
			user_agent TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions(user_id);
		CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions(expires_at);

		CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			key_hash TEXT UNIQUE NOT NULL,
			key_prefix TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			last_used_at TIMESTAMPTZ,
			revoked_at TIMESTAMPTZ
		);
		CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys(user_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS credit_ledger (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			delta BIGINT NOT NULL,
			reason TEXT NOT NULL,
			app_id TEXT NOT NULL DEFAULT 'mojojojo',
			source TEXT NOT NULL,
			balance_after BIGINT NOT NULL CHECK (balance_after >= 0),
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS credit_ledger_user_idx ON credit_ledger(user_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS accrual (
			user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			owed_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
			total_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS runs (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL,
			ms DOUBLE PRECISION NOT NULL,
			gpu BOOLEAN NOT NULL DEFAULT FALSE,
			ok BOOLEAN NOT NULL DEFAULT TRUE,
			credits BIGINT NOT NULL DEFAULT 0,
			native BOOLEAN NOT NULL DEFAULT FALSE,
			source TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS runs_user_idx ON runs(user_id, created_at DESC);
		CREATE TABLE IF NOT EXISTS grants (
			user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			credits BIGINT NOT NULL,
			granted_at TIMESTAMPTZ NOT NULL
		);

		CREATE TABLE IF NOT EXISTS seen (
			hash TEXT PRIMARY KEY,
			ms DOUBLE PRECISION NOT NULL,
			best_ms DOUBLE PRECISION NOT NULL,
			native BOOLEAN NOT NULL DEFAULT FALSE,
			gpu BOOLEAN NOT NULL DEFAULT FALSE,
			runs BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS edge_tickets (
			nonce TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			issued_at TIMESTAMPTZ NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			settled_at TIMESTAMPTZ,
			ms DOUBLE PRECISION NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS edge_tickets_open_idx ON edge_tickets(user_id, settled_at);

		CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			slug TEXT NOT NULL,
			runtime TEXT NOT NULL DEFAULT 'static',
			active_deployment_id TEXT,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (user_id, slug)
		);
		CREATE INDEX IF NOT EXISTS projects_user_idx ON projects(user_id, updated_at DESC);
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT 'app';
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'private';
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS showcase_status TEXT NOT NULL DEFAULT 'draft';
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS featured BOOLEAN NOT NULL DEFAULT FALSE;
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS tags JSONB NOT NULL DEFAULT '[]'::jsonb;
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS daily_call_limit INTEGER NOT NULL DEFAULT 1000;
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS source_revision TIMESTAMPTZ;
		ALTER TABLE projects ADD COLUMN IF NOT EXISTS share_token TEXT;
		CREATE UNIQUE INDEX IF NOT EXISTS projects_share_token_idx ON projects(share_token) WHERE share_token IS NOT NULL;
		UPDATE projects SET source_revision=updated_at WHERE source_revision IS NULL;

		CREATE TABLE IF NOT EXISTS project_files (
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			path TEXT NOT NULL,
			content TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'text/plain; charset=utf-8',
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (project_id, path)
		);
		CREATE INDEX IF NOT EXISTS project_files_project_idx ON project_files(project_id, path);

		CREATE TABLE IF NOT EXISTS project_versions (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			revision TIMESTAMPTZ NOT NULL,
			files JSONB NOT NULL,
			file_count INTEGER NOT NULL,
			size_bytes BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS project_versions_project_idx ON project_versions(project_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS app_function_usage (
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			day DATE NOT NULL,
			calls INTEGER NOT NULL DEFAULT 0,
			billable_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
			PRIMARY KEY (project_id, day)
		);

		CREATE TABLE IF NOT EXISTS app_room_peers (
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			room TEXT NOT NULL,
			peer_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			joined_at TIMESTAMPTZ NOT NULL,
			last_seen_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (project_id, room, peer_id)
		);
		CREATE INDEX IF NOT EXISTS app_room_peers_seen_idx ON app_room_peers(last_seen_at);

		CREATE TABLE IF NOT EXISTS app_room_signals (
			id BIGSERIAL PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			room TEXT NOT NULL,
			sender TEXT NOT NULL,
			to_peer TEXT NOT NULL,
			payload JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS app_room_signals_poll_idx
			ON app_room_signals(project_id, room, id);
		CREATE INDEX IF NOT EXISTS app_room_signals_created_idx ON app_room_signals(created_at);

		CREATE TABLE IF NOT EXISTS showcase_submissions (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			category TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			note TEXT NOT NULL DEFAULT '',
			submitted_at TIMESTAMPTZ NOT NULL,
			reviewed_at TIMESTAMPTZ,
			reviewer_id TEXT REFERENCES users(id) ON DELETE SET NULL
		);
		CREATE INDEX IF NOT EXISTS showcase_status_idx ON showcase_submissions(status, submitted_at DESC);

		CREATE TABLE IF NOT EXISTS deployments (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			status TEXT NOT NULL,
			manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
			entrypoint TEXT NOT NULL DEFAULT 'index.html',
			public_url TEXT NOT NULL DEFAULT '',
			file_count INTEGER NOT NULL DEFAULT 0,
			size_bytes BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS deployments_project_idx ON deployments(project_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS assets (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
			deployment_id TEXT REFERENCES deployments(id) ON DELETE CASCADE,
			object_key TEXT NOT NULL UNIQUE,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
			public_url TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS assets_user_idx ON assets(user_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS assets_project_idx ON assets(project_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS vector_projects (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			slug TEXT NOT NULL,
			tier TEXT NOT NULL DEFAULT 'exact',
			status TEXT NOT NULL DEFAULT 'ready',
			dimension INTEGER NOT NULL DEFAULT 512,
			vector_count BIGINT NOT NULL DEFAULT 0,
			revision BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (user_id, slug)
		);
		CREATE INDEX IF NOT EXISTS vector_projects_user_idx ON vector_projects(user_id, updated_at DESC);
		CREATE TABLE IF NOT EXISTS vector_documents (
			project_id TEXT NOT NULL REFERENCES vector_projects(id) ON DELETE CASCADE,
			external_id TEXT NOT NULL,
			content TEXT NOT NULL,
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (project_id, external_id)
		);
		CREATE TABLE IF NOT EXISTS vector_accrual (
			user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			owed_credits DOUBLE PRECISION NOT NULL DEFAULT 0,
			total_credits DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS vector_usage (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			project_id TEXT REFERENCES vector_projects(id) ON DELETE SET NULL,
			operation TEXT NOT NULL,
			units BIGINT NOT NULL,
			backend TEXT NOT NULL,
			latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
			accrued_credits DOUBLE PRECISION NOT NULL,
			charged_credits BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL
		);

		CREATE TABLE IF NOT EXISTS capacity_reservations (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			product TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('active', 'paused', 'cancelled')),
			credits_per_hour BIGINT NOT NULL CHECK (credits_per_hour > 0),
			current_period_start TIMESTAMPTZ NOT NULL,
			next_charge_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			cancelled_at TIMESTAMPTZ,
			status_reason TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS capacity_reservations_user_idx
			ON capacity_reservations(user_id, created_at DESC);
		CREATE UNIQUE INDEX IF NOT EXISTS capacity_reservations_user_product_open_uq
			ON capacity_reservations(user_id, product)
			WHERE status IN ('active', 'paused');

		CREATE TABLE IF NOT EXISTS agent_accrual (
			user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			owed_credits DOUBLE PRECISION NOT NULL DEFAULT 0,
			total_credits DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS agent_usage (
			run_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			model TEXT NOT NULL,
			tokens BIGINT NOT NULL CHECK (tokens >= 0),
			accrued_credits DOUBLE PRECISION NOT NULL,
			charged_credits BIGINT NOT NULL DEFAULT 0,
			owed_credits_after DOUBLE PRECISION NOT NULL,
			balance_after BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS agent_usage_user_idx ON agent_usage(user_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS vector_usage_user_idx ON vector_usage(user_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS vector_usage_project_idx ON vector_usage(project_id, created_at DESC);

		CREATE TABLE IF NOT EXISTS stripe_customers (
			user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			customer_id TEXT NOT NULL UNIQUE,
			refund_debt_credits BIGINT NOT NULL DEFAULT 0 CHECK (refund_debt_credits >= 0),
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS stripe_checkouts (
			session_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			offer TEXT NOT NULL,
			mode TEXT NOT NULL CHECK (mode IN ('payment', 'subscription')),
			credits BIGINT NOT NULL DEFAULT 0 CHECK (credits >= 0),
			expected_subtotal BIGINT NOT NULL DEFAULT 0 CHECK (expected_subtotal >= 0),
			amount_total BIGINT NOT NULL DEFAULT 0 CHECK (amount_total >= 0),
			amount_refunded BIGINT NOT NULL DEFAULT 0 CHECK (amount_refunded >= 0),
			payment_intent_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			created_at TIMESTAMPTZ NOT NULL,
			completed_at TIMESTAMPTZ
		);
		CREATE INDEX IF NOT EXISTS stripe_checkouts_user_idx
			ON stripe_checkouts(user_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS stripe_checkouts_payment_intent_idx
			ON stripe_checkouts(payment_intent_id) WHERE payment_intent_id <> '';
		CREATE TABLE IF NOT EXISTS stripe_credit_grants (
			external_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			credits BIGINT NOT NULL CHECK (credits >= 0),
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS stripe_subscriptions (
			subscription_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			customer_id TEXT NOT NULL,
			offer TEXT NOT NULL,
			price_id TEXT NOT NULL,
			status TEXT NOT NULL,
			credits_per_period BIGINT NOT NULL DEFAULT 0 CHECK (credits_per_period >= 0),
			current_period_end TIMESTAMPTZ,
			latest_invoice_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE INDEX IF NOT EXISTS stripe_subscriptions_user_idx
			ON stripe_subscriptions(user_id, updated_at DESC);

		CREATE TABLE IF NOT EXISTS usage_meters (
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			meter TEXT NOT NULL,
			month TEXT NOT NULL,
			qty DOUBLE PRECISION NOT NULL DEFAULT 0,
			owed_micro DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (user_id, meter, month)
		);
		CREATE TABLE IF NOT EXISTS data_namespaces (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			slug TEXT NOT NULL,
			vector_project_id TEXT REFERENCES vector_projects(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (user_id, slug)
		);
		CREATE INDEX IF NOT EXISTS data_namespaces_user_idx ON data_namespaces(user_id, updated_at DESC);
		CREATE TABLE IF NOT EXISTS data_records (
			namespace_id TEXT NOT NULL REFERENCES data_namespaces(id) ON DELETE CASCADE,
			record_key TEXT NOT NULL,
			value JSONB NOT NULL,
			search_text TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (namespace_id, record_key)
		);
		CREATE INDEX IF NOT EXISTS data_records_updated_idx ON data_records(namespace_id, updated_at DESC);
	`)
	if err != nil {
		return err
	}
	return migrateCreatorEarnings(db)
}
