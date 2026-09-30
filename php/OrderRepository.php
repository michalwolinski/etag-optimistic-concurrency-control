<?php

declare(strict_types=1);

namespace Shop\Ordering;

use PDO;

final class OrderRepository
{
    public function __construct(private readonly PDO $pdo)
    {
    }

    public function find(int $id): ?Order
    {
        $stmt = $this->pdo->prepare(
            'SELECT id, status, total_minor, version FROM orders WHERE id = :id'
        );
        $stmt->execute(['id' => $id]);
        $row = $stmt->fetch(PDO::FETCH_ASSOC);

        return $row === false ? null : $this->hydrate($row);
    }

    /**
     * Compare-and-set in a single statement. The version is checked and
     * incremented by the database, inside the same atomic write, so no read
     * of ours can go stale between the check and the update.
     *
     * @throws OptimisticLockException
     */
    public function save(Order $order): Order
    {
        $stmt = $this->pdo->prepare(
            'UPDATE orders
                SET status = :status, version = version + 1
              WHERE id = :id AND version = :expected_version'
        );
        $stmt->execute([
            'status' => $order->status,
            'id' => $order->id,
            'expected_version' => $order->version,
        ]);

        // rowCount()==1 means "this statement matched and moved the row".
        // Because the statement always writes version + 1, a matched row is
        // always a changed row - which is what makes this reliable on MySQL,
        // whose driver reports changed rows rather than matched rows by
        // default (PDO::MYSQL_ATTR_FOUND_ROWS is off).
        if ($stmt->rowCount() !== 1) {
            throw new OptimisticLockException($order->id, $order->version, $this->version($order->id));
        }

        return new Order($order->id, $order->status, $order->totalMinor, $order->version + 1);
    }

    private function version(int $id): int
    {
        $stmt = $this->pdo->prepare('SELECT version FROM orders WHERE id = :id');
        $stmt->execute(['id' => $id]);
        $version = $stmt->fetchColumn();

        return $version === false ? 0 : (int) $version;
    }

    /** @param array<string, mixed> $row */
    private function hydrate(array $row): Order
    {
        return new Order(
            (int) $row['id'],
            (string) $row['status'],
            (int) $row['total_minor'],
            (int) $row['version'],
        );
    }
}
