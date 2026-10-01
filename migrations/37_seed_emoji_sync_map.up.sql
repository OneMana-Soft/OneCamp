INSERT INTO emoji_sync_map (github_name, onecamp_uuid) VALUES
    ('+1',    '+1'),
    ('-1',    '-1'),
    ('laugh', 'grinning'),
    ('hooray','tada'),
    ('confused','confused'),
    ('heart', 'heart'),
    ('rocket','rocket'),
    ('eyes',  'eyes')
ON CONFLICT (github_name) DO NOTHING;
