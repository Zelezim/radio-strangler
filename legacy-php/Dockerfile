FROM php:8.3-cli-alpine

# Compilers and headers are installed as a virtual package and removed in the same layer, so the
# final image keeps only the runtime libpq that pdo_pgsql links against.
RUN apk add --no-cache libpq \
 && apk add --no-cache --virtual .build-deps $PHPIZE_DEPS libpq-dev \
 && docker-php-ext-install pdo_pgsql \
 && apk del .build-deps

WORKDIR /app
COPY index.php .

USER www-data

# The built-in server is single-threaded by default; workers let it serve concurrent requests
# (shadow traffic doubles the load on some routes).
ENV PORT=8081 \
    PHP_CLI_SERVER_WORKERS=4
EXPOSE 8081

# sh -c expands ${PORT} at runtime (Render injects its own); exec makes php PID 1 so it receives
# SIGTERM directly and stops cleanly.
CMD ["sh", "-c", "exec php -S 0.0.0.0:${PORT} index.php"]
