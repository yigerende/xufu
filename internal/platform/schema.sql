CREATE TABLE IF NOT EXISTS users (
 id text PRIMARY KEY, email text UNIQUE NOT NULL, name text NOT NULL, password_hash text NOT NULL,
 role text NOT NULL DEFAULT 'member' CHECK(role IN ('member','admin')), status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled')),
 balance bigint NOT NULL DEFAULT 0 CHECK(balance>=0), frozen bigint NOT NULL DEFAULT 0 CHECK(frozen>=0),
 fee_override bigint CHECK(fee_override>=0), invite_code text UNIQUE NOT NULL, invited_by text REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS sessions (hash text PRIMARY KEY,user_id text REFERENCES users(id) ON DELETE CASCADE,expires_at timestamptz NOT NULL);
CREATE TABLE IF NOT EXISTS api_keys (id text PRIMARY KEY,user_id text NOT NULL REFERENCES users(id),name text NOT NULL,hash text UNIQUE NOT NULL,prefix text NOT NULL,revoked boolean NOT NULL DEFAULT false,last_used timestamptz,created_at timestamptz DEFAULT now());
CREATE TABLE IF NOT EXISTS settings (key text PRIMARY KEY,value jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS products (id text PRIMARY KEY,name text NOT NULL,description text NOT NULL,fee bigint NOT NULL CHECK(fee>=0),enabled boolean NOT NULL DEFAULT true,sort int NOT NULL DEFAULT 0,tag text NOT NULL DEFAULT '',features jsonb NOT NULL DEFAULT '[]');
CREATE TABLE IF NOT EXISTS orders (
 id text PRIMARY KEY,user_id text NOT NULL REFERENCES users(id),product_id text NOT NULL REFERENCES products(id),product_name text NOT NULL,
 email text NOT NULL,country text NOT NULL DEFAULT 'PH',seats int NOT NULL DEFAULT 1 CHECK(seats BETWEEN 1 AND 100),
 status text NOT NULL DEFAULT 'draft' CHECK(status IN ('draft','queued','processing','awaiting_payment','succeeded','failed','cancelled','review')),
 fee bigint NOT NULL CHECK(fee>=0),source text NOT NULL DEFAULT 'web',note text NOT NULL DEFAULT '',renewal text NOT NULL DEFAULT 'not_started',
 idempotency_key text NOT NULL,request_hash text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),UNIQUE(user_id,idempotency_key)
);
CREATE INDEX IF NOT EXISTS orders_user_time ON orders(user_id,created_at DESC);
CREATE TABLE IF NOT EXISTS order_events (id bigserial PRIMARY KEY,order_id text NOT NULL REFERENCES orders(id),stage text NOT NULL,message text NOT NULL,created_at timestamptz DEFAULT now());
CREATE TABLE IF NOT EXISTS ledger (
 id bigserial PRIMARY KEY,user_id text NOT NULL REFERENCES users(id),kind text NOT NULL,amount bigint NOT NULL,balance_after bigint NOT NULL,frozen_after bigint NOT NULL,
 reference text NOT NULL,description text NOT NULL,actor_id text REFERENCES users(id),created_at timestamptz NOT NULL DEFAULT now(),UNIQUE(user_id,reference)
);
CREATE INDEX IF NOT EXISTS ledger_user_time ON ledger(user_id,created_at DESC);
CREATE TABLE IF NOT EXISTS topups (id text PRIMARY KEY,user_id text NOT NULL REFERENCES users(id),amount bigint NOT NULL CHECK(amount>0),method text NOT NULL DEFAULT 'manual',proof text NOT NULL DEFAULT '',status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected','cancelled')),note text NOT NULL DEFAULT '',reviewed_by text REFERENCES users(id),created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS redeem_codes (id text PRIMARY KEY,code_hash text UNIQUE NOT NULL,code_enc text NOT NULL,prefix text NOT NULL,amount bigint NOT NULL CHECK(amount>0),batch text NOT NULL,status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','used','disabled')),used_by text REFERENCES users(id),used_at timestamptz,expires_at timestamptz NOT NULL,created_at timestamptz DEFAULT now());
CREATE TABLE IF NOT EXISTS announcements (id text PRIMARY KEY,title text NOT NULL,body text NOT NULL,tone text NOT NULL DEFAULT 'info',published boolean NOT NULL DEFAULT false,login_visible boolean NOT NULL DEFAULT false,sort int NOT NULL DEFAULT 0,starts_at timestamptz,ends_at timestamptz,created_at timestamptz DEFAULT now());
CREATE TABLE IF NOT EXISTS resources (id text PRIMARY KEY,name text NOT NULL,kind text NOT NULL DEFAULT 'proxy',country text NOT NULL DEFAULT 'PH',endpoint_enc text NOT NULL,enabled boolean NOT NULL DEFAULT true,weight int NOT NULL DEFAULT 1,capacity int NOT NULL DEFAULT 1,last_status text NOT NULL DEFAULT 'untested',last_checked timestamptz,created_at timestamptz DEFAULT now());
-- Empty vendor is a migration marker; startup fills it using the prior host rule.
ALTER TABLE resources ADD COLUMN IF NOT EXISTS vendor text NOT NULL DEFAULT '' CHECK (vendor IN ('','iproyal','cliproxy','other'));
ALTER TABLE resources ADD COLUMN IF NOT EXISTS ip_mode text NOT NULL DEFAULT 'dynamic' CHECK (ip_mode IN ('dynamic','static'));
CREATE TABLE IF NOT EXISTS risk_events (id bigserial PRIMARY KEY,user_id text REFERENCES users(id),category text NOT NULL,severity text NOT NULL,ip text NOT NULL,detail text NOT NULL,resolved boolean NOT NULL DEFAULT false,created_at timestamptz DEFAULT now());
CREATE INDEX IF NOT EXISTS risk_time ON risk_events(created_at DESC);
CREATE TABLE IF NOT EXISTS audit_events (id bigserial PRIMARY KEY,actor_id text REFERENCES users(id),action text NOT NULL,target text NOT NULL,detail text NOT NULL,ip text NOT NULL,created_at timestamptz DEFAULT now());
CREATE TABLE IF NOT EXISTS rate_buckets (key text PRIMARY KEY,hits int NOT NULL,expires_at timestamptz NOT NULL);
CREATE TABLE IF NOT EXISTS blocked_ips (ip text PRIMARY KEY,reason text NOT NULL,expires_at timestamptz NOT NULL,created_at timestamptz DEFAULT now());
CREATE TABLE IF NOT EXISTS mutation_keys (key text PRIMARY KEY,actor_id text NOT NULL,request_hash text NOT NULL,result jsonb NOT NULL,created_at timestamptz DEFAULT now());
CREATE TABLE IF NOT EXISTS jobs (
 id text PRIMARY KEY,order_id text UNIQUE NOT NULL REFERENCES orders(id),user_id text NOT NULL REFERENCES users(id),
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','running','completed','failed','review')),
 attempts int NOT NULL DEFAULT 0,max_attempts int NOT NULL DEFAULT 3,next_at timestamptz NOT NULL DEFAULT now(),
 lease_until timestamptz,lease_token text,side_effect_started boolean NOT NULL DEFAULT false,error_code text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(state,next_at,created_at);
ALTER TABLE users ADD COLUMN IF NOT EXISTS membership_code text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS membership_until timestamptz;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS price_snapshot jsonb NOT NULL DEFAULT '{}';
ALTER TABLE ledger ADD COLUMN IF NOT EXISTS order_id text REFERENCES orders(id);
CREATE INDEX IF NOT EXISTS ledger_order_membership ON ledger(user_id,order_id) WHERE kind IN ('charge','refund');
ALTER TABLE resources ADD COLUMN IF NOT EXISTS last_ip text NOT NULL DEFAULT '';
ALTER TABLE resources ADD COLUMN IF NOT EXISTS last_country text NOT NULL DEFAULT '';
ALTER TABLE resources ADD COLUMN IF NOT EXISTS last_latency_ms bigint NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS proxy_leases (
 id text PRIMARY KEY, resource_id text NOT NULL REFERENCES resources(id),
 expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS proxy_leases_capacity ON proxy_leases(resource_id,expires_at);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS billing_address jsonb NOT NULL DEFAULT '{}';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'reserved';
CREATE TABLE IF NOT EXISTS checkout_runs (
 order_id text PRIMARY KEY REFERENCES orders(id), credentials_enc text NOT NULL DEFAULT '',
 credentials_expires_at timestamptz NOT NULL, hosted_url_enc text NOT NULL DEFAULT '',
 link_expires_at timestamptz, proxy_lease_id text, resource_id text REFERENCES resources(id) ON DELETE SET NULL,
 workspace_name text NOT NULL DEFAULT 'My workspace', premium_seats int NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now()
);
-- Card numbers, CVV, raw browser state and upstream bodies are never stored.
ALTER TABLE checkout_runs ADD COLUMN IF NOT EXISTS flow text NOT NULL DEFAULT 'legacy' CHECK(flow IN ('legacy','cslive'));
ALTER TABLE checkout_runs ADD COLUMN IF NOT EXISTS checkout_type text NOT NULL DEFAULT 'hosted' CHECK(checkout_type IN ('hosted','oaics'));
ALTER TABLE checkout_runs DROP CONSTRAINT IF EXISTS checkout_runs_checkout_type_check;
ALTER TABLE checkout_runs ADD CONSTRAINT checkout_runs_checkout_type_check CHECK(checkout_type IN ('hosted','oaics') OR (flow='cslive' AND checkout_type='auto'));
CREATE TABLE IF NOT EXISTS payment_runs (
 order_id text PRIMARY KEY REFERENCES orders(id), attempt_id text UNIQUE NOT NULL,
 state text NOT NULL CHECK(state IN ('preparing','quoted','submitting','checking','action_required','review','succeeded','failed','cancelled')),
 amount bigint CHECK(amount>=0), currency text NOT NULL DEFAULT '', last4 text NOT NULL DEFAULT '',
 quote_expires_at timestamptz, expires_at timestamptz NOT NULL,
 submitted boolean NOT NULL DEFAULT false, fee_state text NOT NULL DEFAULT 'held' CHECK(fee_state IN ('held','released','charged')),
 hosted_url_enc text NOT NULL, error_code text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payment_runs_active ON payment_runs(state,expires_at);
ALTER TABLE payment_runs ADD COLUMN IF NOT EXISTS approval_started boolean NOT NULL DEFAULT false;
ALTER TABLE payment_runs ADD COLUMN IF NOT EXISTS legacy_checkpoint_enc text NOT NULL DEFAULT '';
ALTER TABLE payment_runs ADD COLUMN IF NOT EXISTS executor text NOT NULL DEFAULT 'browser' CHECK (executor IN ('browser','legacy_http'));
ALTER TABLE orders ADD COLUMN IF NOT EXISTS workflow text NOT NULL DEFAULT 'checkout';
ALTER TABLE orders ADD COLUMN IF NOT EXISTS keep_renewal boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS subscription_runs (
 order_id text PRIMARY KEY REFERENCES orders(id), max_amount bigint NOT NULL CHECK(max_amount>0), currency text NOT NULL,
 credentials_enc text NOT NULL DEFAULT '', credentials_expires_at timestamptz NOT NULL,
 renewal_state text NOT NULL DEFAULT 'not_started', cancel_started boolean NOT NULL DEFAULT false,
 checks int NOT NULL DEFAULT 0, next_at timestamptz NOT NULL DEFAULT now(), lease_token text, lease_until timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now()
);
-- Independent, free renewal requests. Provider credentials are never projected.
CREATE TABLE IF NOT EXISTS renewal_tasks (
 id text PRIMARY KEY,user_id text NOT NULL REFERENCES users(id),email text NOT NULL,account_hash text NOT NULL,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','processing','checking','disabled','no_subscription','review')),
 message text NOT NULL DEFAULT '申请已受理，等待处理。',
 credentials_enc text NOT NULL,credentials_expires_at timestamptz NOT NULL,
 cancel_started boolean NOT NULL DEFAULT false,checks int NOT NULL DEFAULT 0,
 next_at timestamptz NOT NULL DEFAULT now(),lease_token text,lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS renewal_account_pending ON renewal_tasks(account_hash) WHERE status IN ('queued','processing','checking','review');
CREATE INDEX IF NOT EXISTS renewal_tasks_queue ON renewal_tasks(next_at) WHERE status IN ('queued','processing','checking');
CREATE TABLE IF NOT EXISTS renewal_request_keys (
 user_id text NOT NULL REFERENCES users(id),key text NOT NULL,account_hash text NOT NULL,
 task_id text NOT NULL REFERENCES renewal_tasks(id),PRIMARY KEY(user_id,key)
);
CREATE TABLE IF NOT EXISTS renewal_events (
 id bigserial PRIMARY KEY,task_id text NOT NULL REFERENCES renewal_tasks(id),
 status text NOT NULL,message text NOT NULL,created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE subscription_runs ADD COLUMN IF NOT EXISTS account_hash text NOT NULL DEFAULT '';
ALTER TABLE subscription_runs ADD COLUMN IF NOT EXISTS renewal_task_id text REFERENCES renewal_tasks(id);
-- Payment success is independent of a terminal failure to close renewal.
WITH repaired AS (
 UPDATE orders o SET status='succeeded',updated_at=now()
 FROM payment_runs p,subscription_runs s
 WHERE o.id=p.order_id AND o.id=s.order_id AND o.workflow='subscription'
 AND o.status='review' AND o.renewal='review' AND p.state='succeeded'
 AND s.renewal_state='review' AND s.lease_token IS NULL
 RETURNING o.id
)
INSERT INTO order_events(order_id,stage,message)
SELECT id,'renewal','订阅成功，取消自动续费失败。请在续费管理粘贴该账号的新 Session 继续处理，无需重新付款。' FROM repaired;
ALTER TABLE resources ADD COLUMN IF NOT EXISTS quality_result jsonb NOT NULL DEFAULT '{}';
ALTER TABLE resources ADD COLUMN IF NOT EXISTS openai_result jsonb NOT NULL DEFAULT '{}';
CREATE TABLE IF NOT EXISTS proxy_checks (
 id text PRIMARY KEY,resource_id text NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('quality','openai')),state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','running','completed')),
 result jsonb NOT NULL DEFAULT '{}',lease_token text,lease_until timestamptz,proxy_lease_id text,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS proxy_checks_pending ON proxy_checks(resource_id) WHERE state IN ('queued','running');

-- Private per-order billing snapshot; never included in order API projections.
CREATE TABLE IF NOT EXISTS order_billing (
 order_id text PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
 details_enc text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);

-- Private order-level proxy snapshot; never projected in customer/admin APIs.
CREATE TABLE IF NOT EXISTS order_proxy_sessions (
 order_id text PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
 resource_id text REFERENCES resources(id) ON DELETE SET NULL,
 endpoint_enc text NOT NULL, expected_ip text NOT NULL DEFAULT '',
 expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE payment_runs ADD COLUMN IF NOT EXISTS network_restarts int NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS payment_trace_events (
 id bigserial PRIMARY KEY,order_id text NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
 phase text NOT NULL,state text NOT NULL,code text NOT NULL DEFAULT '',round int NOT NULL,
 elapsed_ms bigint NOT NULL DEFAULT 0,created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payment_trace_order ON payment_trace_events(order_id,id);
ALTER TABLE payment_trace_events ADD COLUMN IF NOT EXISTS http_status int NOT NULL DEFAULT 0;
ALTER TABLE payment_trace_events ADD COLUMN IF NOT EXISTS request_id text NOT NULL DEFAULT '';
ALTER TABLE payment_trace_events ADD COLUMN IF NOT EXISTS diagnostics jsonb NOT NULL DEFAULT '{}';
ALTER TABLE orders ADD COLUMN IF NOT EXISTS activation text NOT NULL DEFAULT 'not_checked';
ALTER TABLE subscription_runs ADD COLUMN IF NOT EXISTS activation_checks int NOT NULL DEFAULT 0;
ALTER TABLE payment_runs ADD COLUMN IF NOT EXISTS payment_evidence_enc text NOT NULL DEFAULT '';
ALTER TABLE payment_runs ADD COLUMN IF NOT EXISTS charged_amount bigint;
ALTER TABLE payment_runs ADD COLUMN IF NOT EXISTS charged_currency text NOT NULL DEFAULT '';

-- Checkout history is encrypted and only projected to administrator order details.
CREATE TABLE IF NOT EXISTS order_checkout_links (
 order_id text NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
 round int NOT NULL CHECK(round>0),url_enc text NOT NULL,
 generated_at timestamptz,expires_at timestamptz,
 recorded_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(order_id,round)
);
-- Preserve only links that still exist. Do not invent generation times for
-- legacy orders or restore credentials that were already cleared.
INSERT INTO order_checkout_links(order_id,round,url_enc,expires_at)
 SELECT c.order_id,COALESCE(p.network_restarts,0)+1,
 COALESCE(NULLIF(c.hosted_url_enc,''),NULLIF(p.hosted_url_enc,'')),c.link_expires_at
 FROM checkout_runs c LEFT JOIN payment_runs p ON p.order_id=c.order_id
 WHERE COALESCE(NULLIF(c.hosted_url_enc,''),NULLIF(p.hosted_url_enc,'')) IS NOT NULL
 ON CONFLICT(order_id,round) DO NOTHING;

CREATE TABLE IF NOT EXISTS subscription_protocol (
 order_id text PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
 checkpoint_enc text NOT NULL DEFAULT '',
 phase text NOT NULL DEFAULT 'queued',
 lease_token text, lease_until timestamptz,
 expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours',
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- Manual local fulfillment closure preserves the prior provider result category.
CREATE TABLE IF NOT EXISTS order_closures (
 order_id text PRIMARY KEY REFERENCES orders(id),actor_id text NOT NULL REFERENCES users(id),
 reason text NOT NULL,prior_status text NOT NULL,prior_payment_state text NOT NULL,payment_submitted boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
