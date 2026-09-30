<?php

declare(strict_types=1);

require __DIR__ . '/Order.php';
require __DIR__ . '/OptimisticLockException.php';
require __DIR__ . '/OrderRepository.php';

use Shop\Ordering\Order;
use Shop\Ordering\OrderRepository;
use Shop\Ordering\OptimisticLockException;

$pdo = new PDO('sqlite::memory:', null, null, [
    PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
]);
$pdo->exec(
    'CREATE TABLE orders (
        id          INTEGER PRIMARY KEY,
        status      TEXT    NOT NULL,
        total_minor INTEGER NOT NULL,
        version     INTEGER NOT NULL
    )'
);
$pdo->exec("INSERT INTO orders (id, status, total_minor, version) VALUES (1, 'new', 4999, 1)");

$orders = new OrderRepository($pdo);

echo "=== 1. Two writers hold the same read of the same row ===\n";

$alice = $orders->find(1);
$bob = $orders->find(1);
printf("alice read version %d, bob read version %d\n\n", $alice->version, $bob->version);

$aliceAfter = $orders->save($alice->withStatus('paid'));
printf(
    "alice  PUT status=paid     -> %d row, version %d -> %d\n",
    1,
    $alice->version,
    $aliceAfter->version,
);

try {
    $orders->save($bob->withStatus('shipped'));
    echo "bob    PUT status=shipped  -> committed (this is the lost update)\n";
} catch (OptimisticLockException $e) {
    printf("bob    PUT status=shipped  -> rejected: %s\n", $e->getMessage());
}

$winner = $orders->find(1);
printf("\nrow now holds status=%s, version=%d\n", $winner->status, $winner->version);
printf("strong validator the transport layer can mint: \"order-%d-v%d\"\n", $winner->id, $winner->version);

echo "\n=== 2. Hash of the current state as the validator ===\n";

$pdo->exec('CREATE TABLE orders_hashed (id INTEGER PRIMARY KEY, status TEXT NOT NULL, content_hash TEXT NOT NULL)');
$hashOf = static fn (string $status): string => substr(hash('sha256', $status), 0, 12);
$pdo->prepare('INSERT INTO orders_hashed (id, status, content_hash) VALUES (1, ?, ?)')
    ->execute(['new', $hashOf('new')]);

$staleWriterHash = $hashOf('new');
printf("alice read state 'new', validator %s\n", $staleWriterHash);

$casByHash = $pdo->prepare('UPDATE orders_hashed SET status = ?, content_hash = ? WHERE id = 1 AND content_hash = ?');
$casByVersion = $pdo->prepare('UPDATE orders SET status = ?, version = version + 1 WHERE id = 1 AND version = ?');

$casByHash->execute(['shipped', $hashOf('shipped'), $staleWriterHash]);
$casByHash->execute(['new', $hashOf('new'), $hashOf('shipped')]);
echo "carol  PUT new->shipped    -> committed\n";
echo "carol  PUT shipped->new    -> committed (state is 'new' again, two commits later)\n\n";

$casByHash->execute(['cancelled', $hashOf('cancelled'), $staleWriterHash]);
printf(
    "alice  PUT status=cancelled -> %d row changed; validator still matched, stale write accepted\n",
    $casByHash->rowCount(),
);

$casByVersion->execute(['cancelled', 1]);
printf(
    "alice  PUT status=cancelled -> %d rows changed when guarded by version %d instead\n",
    $casByVersion->rowCount(),
    1,
);

echo "\ndone\n";
