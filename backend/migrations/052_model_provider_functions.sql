-- Models of non-default AI providers are stored as "provider::model" in
-- model policies, user preferences and usage records. These helpers let the
-- catalog join such an id to provider_models(provider, model_id) and to
-- provider_cost_rates(provider, sku) without changing those tables. Ids
-- without a separator belong to the default provider.
CREATE OR REPLACE FUNCTION model_provider(qualified TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE
    WHEN qualified ~ '^[a-z0-9][a-z0-9-]{0,39}::' THEN split_part(qualified, '::', 1)
    ELSE 'openai-compatible'
  END
$$;

CREATE OR REPLACE FUNCTION model_sku(qualified TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE
    WHEN qualified ~ '^[a-z0-9][a-z0-9-]{0,39}::' THEN substr(qualified, position('::' in qualified) + 2)
    ELSE qualified
  END
$$;
