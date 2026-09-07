CREATE TABLE admin_roles (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 key VARCHAR(60) UNIQUE NOT NULL,
 name VARCHAR(100) NOT NULL,
 permissions JSONB NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(permissions)='array'),
 channels JSONB NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(channels)='array'),
 builtin BOOLEAN NOT NULL DEFAULT FALSE,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE users ADD COLUMN admin_role_id UUID REFERENCES admin_roles(id) ON DELETE RESTRICT;
INSERT INTO admin_roles(key,name,permissions,builtin) VALUES
 ('super_admin','超级管理员','["*"]',true),
 ('marketing','运营 / 营销','["dashboard.read","promotions.read","promotions.write","codes.read","codes.write","announcements.read","announcements.write"]',true),
 ('finance','财务','["dashboard.read","finance.read","pricing.read","audit.read","export","settlements.review"]',true),
 ('technical','技术','["dashboard.read","metrics.read","models.read","models.write","pricing.read","routing.read","routing.write"]',true),
 ('agent','代理','["agent.self"]',true);
