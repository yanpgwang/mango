// Synthetic inventory for this example's disposable MongoDB container.
db.getSiblingDB('mango_example').inventory.insertMany([
  { sku: 'TEA', name: 'Green tea', stock: 4, reorder_point: 12 },
  { sku: 'COFFEE', name: 'Coffee beans', stock: 18, reorder_point: 10 },
  { sku: 'MUG', name: 'Ceramic mug', stock: 2, reorder_point: 6 },
]);
