<?php
/*
 * Southern Cross FM - Station API (legacy)
 *
 * NOTE: this file is intentionally written in an old-school, procedural PHP style: one file,
 * global functions, no framework, no autoloader, a new database connection per request.
 * It plays the role of the legacy system that radio-strangler migrates to Go, and its JSON
 * output is the contract the Go implementation must reproduce. Do not "modernize" it.
 *
 * Runs on PHP's built-in web server, with this file as the router script:
 *   php -S 0.0.0.0:8081 index.php
 */

function send_json($status, $payload, $headers = array())
{
    http_response_code($status);
    header('Content-Type: application/json');
    foreach ($headers as $name => $value) {
        header($name . ': ' . $value);
    }
    echo json_encode($payload);
    exit;
}

function db()
{
    static $pdo = null;
    if ($pdo !== null) {
        return $pdo;
    }

    $url = getenv('DATABASE_URL');
    if (!$url) {
        throw new Exception('DATABASE_URL is not set');
    }
    $parts = parse_url($url);
    if ($parts === false || !isset($parts['host'])) {
        throw new Exception('DATABASE_URL is not a valid URL');
    }

    $dbname = isset($parts['path']) ? ltrim($parts['path'], '/') : '';
    if ($dbname === '') {
        $dbname = 'postgres';
    }
    $dsn = 'pgsql:host=' . $parts['host']
        . ';port=' . (isset($parts['port']) ? $parts['port'] : 5432)
        . ';dbname=' . $dbname;

    if (isset($parts['query'])) {
        parse_str($parts['query'], $query);
        if (isset($query['sslmode'])) {
            $dsn .= ';sslmode=' . $query['sslmode'];
        }
    }

    $user = isset($parts['user']) ? rawurldecode($parts['user']) : null;
    $pass = isset($parts['pass']) ? rawurldecode($parts['pass']) : null;

    $pdo = new PDO($dsn, $user, $pass, array(
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
        PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
    ));
    return $pdo;
}

function format_track($row)
{
    return array(
        'id' => (int) $row['id'],
        'title' => $row['title'],
        'artist' => $row['artist'],
        'album' => $row['album'],
        'duration_seconds' => (int) $row['duration_seconds'],
        'tags' => json_decode($row['tags'], true),
    );
}

function handle_healthz()
{
    send_json(200, array('status' => 'ok'));
}

function handle_programs()
{
    $stmt = db()->query(
        "SELECT p.id, p.name, p.host, p.weekday,
                to_char(p.start_time, 'HH24:MI') AS start_time,
                to_char(p.end_time, 'HH24:MI') AS end_time
           FROM programs p
          ORDER BY p.weekday, p.start_time, p.id"
    );
    $data = array();
    foreach ($stmt->fetchAll() as $row) {
        $data[] = array(
            'id' => (int) $row['id'],
            'name' => $row['name'],
            'host' => $row['host'],
            'weekday' => (int) $row['weekday'],
            'start_time' => $row['start_time'],
            'end_time' => $row['end_time'],
        );
    }
    send_json(200, array('data' => $data));
}

function handle_tracks()
{
    $stmt = db()->query(
        "SELECT id, title, artist, album, duration_seconds, array_to_json(tags) AS tags
           FROM tracks
          ORDER BY id"
    );
    $data = array();
    foreach ($stmt->fetchAll() as $row) {
        $data[] = format_track($row);
    }
    send_json(200, array('data' => $data));
}

function handle_now_playing()
{
    $stmt = db()->query(
        "SELECT t.id, t.title, t.artist, t.album, t.duration_seconds, array_to_json(t.tags) AS tags,
                to_char(pl.played_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') AS started_at
           FROM plays pl
           JOIN tracks t ON t.id = pl.track_id
          ORDER BY pl.played_at DESC, pl.id DESC
          LIMIT 1"
    );
    $row = $stmt->fetch();

    send_json(200, array(
        'track' => $row ? format_track($row) : null,
        'started_at' => $row ? $row['started_at'] : null,
        'generated_at' => gmdate('Y-m-d\TH:i:s\Z'),
    ));
}

$routes = array(
    '/healthz' => 'handle_healthz',
    '/api/programs' => 'handle_programs',
    '/api/tracks' => 'handle_tracks',
    '/api/now-playing' => 'handle_now_playing',
);

$path = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);

if (!isset($routes[$path])) {
    send_json(404, array('error' => 'not found'));
}
if ($_SERVER['REQUEST_METHOD'] !== 'GET') {
    send_json(405, array('error' => 'method not allowed'), array('Allow' => 'GET'));
}

try {
    $routes[$path]();
} catch (Throwable $e) {
    // Details go to the server log only; clients get a generic message.
    error_log('[' . $path . '] ' . get_class($e) . ': ' . $e->getMessage());
    send_json(500, array('error' => 'internal error'));
}
