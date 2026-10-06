-- Demo data for "Southern Cross FM", a fictional Australian community radio station.
-- The edge cases are deliberate: they are exactly where a rewrite tends to drift from the
-- legacy contract (null vs missing, [] vs null, apostrophes in text).

INSERT INTO programs (name, host, weekday, start_time, end_time) VALUES
    ('Sunday Sessions',     'Liam Walker',  0, '10:00', '13:00'),
    ('Bondi Breakfast',     'Mia Thompson', 1, '06:00', '09:00'),
    ('Outback Drive',       'Jack O''Brien', 1, '16:00', '18:00'),
    ('Harbour Lights Jazz', 'Chloe Nguyen', 3, '20:00', '22:00');

-- "Station Ident #3" (third) has no tags; it and "Red Dirt Highway" have no album.
INSERT INTO tracks (title, artist, album, duration_seconds, tags) VALUES
    ('Southern Lights',           'The Wattle Band',  'Coastal Roads', 245, '{indie,australian}'),
    ('Red Dirt Highway',          'Kirra Jones',      NULL,            198, '{country,australian}'),
    ('Station Ident #3',          'Southern Cross FM', NULL,            12, '{}'),
    ('Midnight at Circular Quay', 'Harbour Quartet',  'Night Ferry',   312, '{jazz,instrumental}');

-- Lookups by title instead of hard-coded ids keep the seed independent of sequence values.
-- The most recent play is the ident, so now-playing exercises the null-album / empty-tags path.
INSERT INTO plays (track_id, played_at)
SELECT t.id, now() - p.ago
FROM (VALUES
        ('Southern Lights',           interval '14 minutes'),
        ('Midnight at Circular Quay', interval '9 minutes'),
        ('Station Ident #3',          interval '1 minute')
     ) AS p (title, ago)
JOIN tracks t ON t.title = p.title;

-- Every route starts on PHP; the two in shadow are being verified against Go.
INSERT INTO route_rules (route, mode, ignore_fields) VALUES
    ('/api/programs',    'legacy', '{}'),
    ('/api/tracks',      'shadow', '{}'),
    ('/api/now-playing', 'shadow', '{generated_at}')
ON CONFLICT (route) DO NOTHING;
